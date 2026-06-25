# kit / workspace / project 構成の再編

ステータス: ドラフト (第 7 ラウンド改訂 — Codex 第 6 ラウンドレビュー反映 / 2026-06-25)

## 背景と動機

現状の boid では、再利用可能であるべき `project.yaml` と `kit.yaml` に環境固有情報が混入している。

- `project.yaml` は git にコミットされる。にもかかわらず `host_commands` / `additional_bindings` / `env` / `kits` / `secret_namespace` を持ち、これらの多くはマシン固有。同僚と共有したとき環境差で壊れる。
- `project.yaml` で `volta` kit を指定すると volta の使用が強制される。プロジェクトとしては「node が使えれば十分」なのに、node の供給手段 (volta / nvm / system) という**環境判断をプロジェクトに焼き付けて**しまう。

根本原因は、project が「何をするか (task_behavior)」と「この環境でどう動かすか (kit / env / binding)」を 1 ファイルに混ぜていること。前者は portable、後者は machine-local。これを 3 レイヤに分離する。

設計方針は「環境固有の結合を*消す*」のではなく、「**commit される側 (project) から uncommitted な machine-local 側 (workspace / kit) へ追い出す**」こと。結合先が workspace / kit に移る時点で、git に環境情報が乗らなくなり、volta 強制も消える。capability の provides/requires 抽象は導入しない (実装軽量化を優先、要求と供給のミスマッチは runtime で顕在化させる)。

## 再編後の責務

責務境界を 3 レイヤに割り直す。

| レイヤ | 所在 | git | 持つもの | 性格 |
|---|---|---|---|---|
| **project** | `.boid/project.yaml` | ✅ commit | id, name, task_behaviors, worktree / base_branch / fork_point | **作業パターン** (portable) |
| **workspace** | `~/.config/boid/workspaces/<slug>.yaml` | ❌ | 有効化する kit 名のリスト, plain env, docker capability | **環境マッチング** (machine-local) |
| **kit** | `~/.local/share/boid/kits/<name>/kit.yaml` | ❌ | meta, host_commands (パス込み), additional_bindings, env | **ツール供給** (machine-local、ワークスペース横断で共有可能) |
| **config.yaml** | `~/.config/boid/` | ❌ | gc, web, notify, sandbox.allowed_domains (daemon 全体・据え置き) | daemon 全体設定 |
| **secret** | DB (AES-GCM) | ❌ | namespace = workspace_id 固定。host_command にのみ `secret:` で注入 | 資格情報 |
| **project↔workspace 紐付け** | DB `project_workspaces` | ❌ | project_id → workspace_id (N:1) | 既存テーブル流用 |

責務分離は **「マシン状態 (kit)」 / 「環境マッチング (workspace)」 / 「作業パターン (project)」** の 3 軸。kit はマシンの「いま使えるツール」のインベントリで複数 workspace から**共有**される。workspace は project とマシン状態を結び付ける場で、project は portable な作業パターンのみ持つ。

## 各レイヤ詳細

### project (`.boid/project.yaml`)

git commit 対象。環境固有の語彙を一切持たない。

**残すフィールド**:

```yaml
id: <uuid>
name: <human-readable>
worktree: true | false
base_branch: main
fork_point: refs/remotes/origin/HEAD  # optional
default_task_behavior: supervisor  # optional, behavior 無指定 task の既定

task_behaviors:
  executor:
    readonly: false
    traits: [...]
    default_instruction:
      message: "..."
      model: opus | sonnet
    # hooks は spec_loader 経由で resolve される (現状踏襲)
  supervisor:
    readonly: true
    traits: [...]
```

worktree / base_branch / fork_point は VCS 構造の話で移植可能なため project 残留。`default_task_behavior` は API リクエストが `behavior` / `behavior_spec` 無指定で来たときのフォールバック先で portable な作業パターン側の概念なので project 残留 (`spec_types.go:438`、`behavior_resolve.go:103`)。task_behaviors が持てるのは readonly / traits / default_instruction / hooks のみ。

**behavior は動的概念、project の `task_behaviors:` は雛形を提供するに留まる**: behavior は task 作成時に決まる動的概念であり、project.yaml の `task_behaviors:` は名前付きの雛形 (プリセット) を提供するにすぎない。API は inline での behavior 指定 (`CreateTaskRequest.behavior_spec`、`internal/api/task.go:170`) を引き続きサポートする。`BehaviorSpec` 型 (`spec_types.go:371`) は**維持**する — kit が behavior を provide する経路の廃止 (本再編のスコープ) と、API での inline behavior 指定機構の存続は独立した話。

**削除キー化するフィールド** (検出 → ガイダンス付きエラー):

- top-level: `kits`, `env`, `host_commands`, `additional_bindings`, `secret_namespace`, `capabilities` (docker 含む)
- behavior-level: `task_behaviors.<name>.kits` (behavior 粒度の kit 指定機能廃止 — 「この behavior は volta で」は環境判断を behavior に焼く行為で本再編が消す対象)

**現行 Go 構造体から削除する型 / フィールド** (`spec_types.go`):

- `ProjectMeta.Kits []KitRef` (line 406)
- `TaskBehavior.Kits []KitRef` (line 355)
- `KitRef.Alias` フィールド (line 379) — workspace.yaml の `kits:` は string[] で alias 廃止

`BehaviorSpec` 型 (line 371-375) は前述のとおり**残す** — API の inline behavior 指定機構として独立に生きており、kit 経路廃止とは無関係。

`Capabilities` / `DockerCapability` struct (line 391-401) も**残す** — `ProjectMeta.Capabilities` フィールドの yaml タグを `yaml:"-"` にして **runtime-only 化**し、workspace.yaml の `capabilities.docker` から後述「workspace runtime → JobSpec 配線」 経路で inject される容れ物として使う。`TaskBehavior` の `Hooks` / `Env` / `HostCommands` / `AdditionalBindings` / `KitRoots` と同じ「runtime merge 容れ物」 パターン。下流 (`planner.go:96` / `cmd/exec.go:88`) の参照箇所は無変更で済む。

`ProjectMeta.SecretNamespace` フィールド (line 431) も同じ流儀で**残す** — yaml タグを `yaml:"-"` にして **runtime-only 化**し、hydrate 時に `project.WorkspaceID` を inject される容れ物として使う。project.yaml に `secret_namespace:` を書く経路は削除キー化 (前述リスト) で塞ぐが、struct フィールドそのものは `planner.go:104` の `SecretNamespace: meta.SecretNamespace` 配線と `runner.go:194` の secret 解決経路の中継として温存する (これが無いと secret が全部 `"default"` namespace にフォールバックする退化が起きる — Codex 第 5 ラウンド指摘 1 対応)。

`TaskBehavior` の `yaml:"-"` 群 (line 359-365: `Hooks`, `Env`, `HostCommands`, `AdditionalBindings`, `KitRoots`) は維持。これらは runtime に kit / workspace から merge された結果を保持する箱として使う。

task_behavior の hook が `gh` / `node` 等を暗黙に要求しても、project はそれを宣言しない (capability 契約を入れないため)。供給は workspace / kit の責務。要求と供給のミスマッチは runtime で顕在化する (事前検出はスコープ外 C)。

### workspace (`~/.config/boid/workspaces/<slug>.yaml`)

git 管理外。machine-local。「このワークスペースの project 群がこのマシンでどう動くか」を kit カタログから選んでマッチングするレイヤ。

**スキーマ**:

```yaml
# ~/.config/boid/workspaces/<slug>.yaml

# 必須: このマシンで有効化する kit 名のリスト。
# kit は ~/.local/share/boid/kits/<name>/kit.yaml で解決される。
kits:
  - node
  - go
  - github-cli

# 任意: plain な環境変数 (secret は含めない)。
# kit 由来 env と merge される (kit が先、workspace が override)。
env:
  EDITOR: vim
  PAGER: less

# 任意: native docker proxy を有効化 (presence = on)。
# 現行 project.yaml の capabilities.docker からの移譲。
capabilities:
  docker: {}
```

**意図的に持たないフィールド**:

- `secret_namespace` — namespace = workspace_id 固定 (後述「secret」節)
- `host_commands` / `additional_bindings` — kit 側で完結。workspace は kit を**参照するだけ**
- `name` / `description` — slug がそのまま識別子。必要になったら後で足す
- `version` — schema 変更は migration loader が吸収するので yaml に versioning は持たない

**設計上の決定**:

- **env merge ルール**: kit env が先に積まれ、workspace env が後で **override** (現行 `MergeKitRuntime` `spec_loader.go:38-47` と同じ「後勝ち」)
- **kits の順序と衝突**: 列挙順 = bind 順 (PATH 含む)。env と `additional_bindings` は後勝ち / union で merge するが、**host_commands の重複は現行どおり error reject** (`MergeKitRuntime` `spec_loader.go:49-64`)。同名 host_command を複数 kit が宣言した場合は、ユーザがどちらかを kit.yaml から消すか workspace.yaml の `kits:` 列挙から外す必要がある。「後勝ち」ではない (override にすると binding/policy がサイレントに差し変わる事故を避けるため、現行挙動を維持)
- **slug がファイル名**: ファイル名 = slug。DB の `project_workspaces.workspace_id` と一致させる

**slug 検証** (path component / URL に乗るため必須):

- 許可: `[a-z0-9-]+`
- 拒否: 空文字 / `/` / `..` / 空白 / 大文字 / `_`
- 長さ: 1 〜 64 文字
- 共通バリデータ `internal/orchestrator/workspace_slug.go` の `ValidWorkspaceSlug(s string) error` を**3 層で共用**:
  - **CLI 入口** (`cmd/workspace.go` の `assign`, `cmd/project.go` の `add --workspace`) — 早期エラー、UX 改善
  - **API 入口** (`internal/api/project.go:SetWorkspace`, `project_service.go:SetProjectWorkspace`) — 400 Bad Request を早く返す
  - **Domain 層** (`internal/orchestrator/project_catalog.go:SetProjectWorkspace`) — **最終防衛** (DB INSERT 直前、ここを通らない経路は無い)

**SoT (Source of Truth) と片方欠落の許容**:

workspace の identity は **slug = yaml ファイル名 = DB `project_workspaces.workspace_id`** で三者一致させる。**yaml と DB の片方だけ存在する状態は許容**する (degraded 窓を意図的に作る):

| 状態 | 発生タイミング | 挙動 |
|---|---|---|
| yaml + DB 両方 | `workspace configure` 完了後の通常状態 | 正常 |
| DB のみ | `project add --workspace foo` で get-or-create した直後 (`workspace configure` 前) | 紐付けはあるが kit / env 供給なし → 後述「degraded 窓」 (`workspace.yaml` 未生成) と一貫した挙動 |
| yaml のみ | `workspace clear` で全 project の紐付けを外した直後 | project 0 件の workspace。`workspace remove` で物理削除可能 |

**権威の分担**:

- **「有効化 kit リスト / env / capabilities」** → workspace.yaml が権威 (kit と env と docker は machine-local な環境マッチング情報なので yaml に閉じる)
- **「project ↔ workspace 紐付け」** → DB `project_workspaces` が権威 (現行どおり、project から workspace を引く SQL 経路を残す)

**WorkspaceStore 責務** (`internal/orchestrator/workspace_store.go` 新規):

- `Load(slug) (*WorkspaceMeta, error)` — yaml 読み + parse + interpolate
- `Save(slug, meta) error` — **tmp + rename で atomic write** (同一 FS、クラッシュ時に半書き yaml を残さない)
- `Remove(slug) error` — 物理削除 (紐付け project がある状態での呼び出しは caller 側 = CLI で reject、`workspace remove` 節参照)
- `List() ([]slug, error)` — yaml ディレクトリ scan

DB 操作 (`project_workspaces` テーブルの read/write) は現状どおり `project_catalog.go` 側に残す。WorkspaceStore は yaml 側だけを責務とし、DB との整合チェックは上位 (CLI / api / domain) で行う。

**Parse error**: workspace.yaml の parse 失敗時は daemon 起動を拒否 + 該当 slug とエラー行を出す (project.yaml 削除キー検出と同じ流儀)。`Load` の戻り値が error の workspace を含む project が dispatch されたら job も即 fail させ、`workspace configure <slug>` の案内を添える。

**`workspace list` / `workspace show` の union 仕様** (Codex 第 4 ラウンド指摘 4 対応):

`workspace list` は **yaml ディレクトリ scan (`WorkspaceStore.List`) と DB scan (`ListWorkspaces`) の union** を取り、各 slug を以下の 3 状態に分類して表示する。片方欠落 = UI から消える事故を防ぐ:

| 状態 | 条件 | 意味 |
|---|---|---|
| `ready` | yaml + DB 両方に存在 | 通常 (`workspace configure` 完了後) |
| `unconfigured` | DB only (project が紐付いているが yaml なし) | `boid workspace configure <slug>` 待ち |
| `empty` | yaml only (yaml はあるが project 紐付け 0 件) | `boid workspace remove <slug>` で削除可能 |

`workspace show <slug>` は片方欠落時に欠落側を明示する (例: 「`workspace.yaml: not found, run` `boid workspace configure <slug>` to create」 / 「project 紐付け 0 件」)。

### kit (`~/.local/share/boid/kits/<name>/kit.yaml`)

git 管理外。machine-local。複数 workspace で**共有可能**なグローバル資源 (= マシン上で利用可能な「ツール供給」のインベントリ)。

**スキーマ**:

```yaml
# ~/.local/share/boid/kits/<name>/kit.yaml

# 必須: 人間可読メタ。kit list 表示と workspace configure のマッチングで使用。
meta:
  name: node
  description: "Node.js via volta"
  category: language    # language / vcs / ci / utility 等

# kit が提供する host_command 群 (パス込み = マシン固有)。
host_commands:
  node:
    path: /home/nosen/.volta/bin/node
  npm:
    path: /home/nosen/.volta/bin/npm
    env:
      NPM_TOKEN: "secret:npm_token"   # secret 注入は env の secret: プレフィックス (現状踏襲)

# kit が要求する bind mount。
additional_bindings:
  - source: /home/nosen/.volta
    target: /home/nosen/.volta
    mode: ro

# kit が provide する env (plain のみ。生 secret 値は書かない)。
env:
  VOLTA_HOME: /home/nosen/.volta
```

**現行 `KitMeta` から削除するフィールド**:

- `detect` (project 検出スクリプト) — `workspace configure` のスキルが project マッチングを代行
- `requires` (PATH に必要なコマンド宣言) — `kit init` が PATH 検出して生成する前提、runtime ミスマッチは runtime で顕在化
- `provides_agent` (agent 提供 kit) — Phase 3-e で adapter (claude / codex / opencode) に移行済み、死フィールド
- `deprecated` — ローカル authored で意味薄い
- `hooks` (top-level kit hooks) — kit が hook を持たない (生成スキル複雑化を避ける)
- `task_behaviors` — kit が behavior を provide する経路を廃止 (責務明確化)

**設計上の決定**:

- kit は**グローバル共有**。複数 workspace から同じ `node` kit を参照していい (= マシン全体で 1 つの kit カタログ)
- **secret 注入**: `host_commands` の `env` で `secret:<key>` プレフィックス (現状の `RegisterWithSecrets` 経路をそのまま再利用)
- **生 secret 値は kit.yaml のどこにも書かない** (= secret-free 規約。後述「secret-free 規約 + 後段 scan」節)
- kit 名の validation: `[a-z0-9-]+` のみ許可。`/` / `..` / `_` / 空白を弾く。workspace slug と style 統一

**`KitRegistry` の simplify** (`kit_registry.go`):

- `Resolve(name) (string, error)` — kit 名 1 経路で `~/.local/share/boid/kits/<name>/kit.yaml` を返す
- `local/` prefix 撤去、`host/owner/repo/kit` 形式撤去 (kit は local オンリーなので 2 経路は冗長)
- `Install` / `InstallFromURL` / `Update` / `RepoRefsFromKitRefs` / `RepoRefToCloneURL` を**削除** (リモート概念消滅)
- `List` / `Remove` / `IsInstalled` を**維持** (CLI から使う)

### secret (DB, AES-GCM)

- **namespace = workspace_id に固定**。workspace.yaml には `secret_namespace` フィールドを持たない
- 同一 workspace の全 project が同じ secret namespace を共有する (退化点だが意図的、後述「退化点と意図的トレードオフ」)
- host_command にのみ `secret:` プレフィックスで注入 (現行 `broker.go` の `RegisterWithSecrets` 経路をそのまま使う)
- **env への自動展開はしない**: 「namespace の secret を自動的に env 有効化」案は不採用。全 secret がエージェント本体と全子プロセスに露出すると blast radius が過大 (commit / PR / log 経由の漏洩面)。workspace.yaml の `env` は plain な値に限る

### config.yaml (据え置き)

`gc` / `web` / `notify` / `sandbox.allowed_domains`。daemon 全体のマシン設定。今回は変更しない。workspace 単位の egress allowlist (スコープ外 E) は保留。

## コマンド体系

kit と workspace は**別々の名詞**配下のコマンド体系として並走する。生成エージェントが入る入口は 2 つ (`boid kit init` / `boid workspace configure`)、それぞれ責務が明確に分かれる。

### `boid kit ...`

| コマンド | 役割 | エージェント |
|---|---|---|
| `boid kit init` | 環境スキャン + 生成スキル参照で `~/.local/share/boid/kits/<name>/kit.yaml` を**まとめて生成**。引数なし固定 | あり (対話セッション) |
| `boid kit list` | インストール済み kit を一覧 (`meta.name` + category + path) | なし |
| `boid kit remove <name>` | kit を物理削除 (`rm -rf ~/.local/share/boid/kits/<name>/`)。**workspace.yaml が参照中ならエラーで止める** | なし |

**廃止コマンド**: `boid kit install [repo]` / `boid kit update <repo>` (リモート概念消滅)

**`boid kit init` の詳細**:

- 「**いまこのマシンで利用できる kit を収拾**」がコンセプト。project は見ない (project マッチングは `workspace configure` の責務)
- **引数なし固定**。個別 target 指定の `--target node` は持たない (1 個だけ作りたいなら手動で kit.yaml 書く)
- 既存 kit との衝突は **対話エージェントが上書き確認**。事前 remove は不要
- 生成スキルは旧 boid-kits の go-dev / github-cli / volta / node / docker 等の kit 雛形を **embed されたリファレンス** として持つ。スキルが環境を検出 (`which volta` / `which gh` / `~/.volta` の dir/binary 確認等) して該当雛形を選び、各マシン用の kit.yaml を書く
- sandbox プロファイル: init 専用 (後述)

### `boid workspace ...`

| コマンド | 役割 | エージェント |
|---|---|---|
| `boid workspace configure <slug>` | workspace 配下の project 群スキャン + kit カタログマッチング → workspace.yaml 生成 | あり (対話セッション) |
| `boid workspace list` | 一覧 (yaml + DB の slug **union**、状態 = `ready` / `unconfigured` / `empty`、紐付け project 数。詳細は前述「`workspace list` / `workspace show` の union 仕様」 節) | なし |
| `boid workspace show <slug>` | workspace.yaml 内容 + 紐付け project リスト表示 (片方欠落時はその旨を明示) | なし |
| `boid workspace assign <project> <slug>` | project を workspace に紐付け (未知 slug は **get-or-create** = DB row 作るだけ、workspace.yaml は configure 待ち) | なし |
| `boid workspace clear <project>` | 紐付け解除 | なし |
| `boid workspace remove <slug>` | workspace.yaml 削除 + DB 紐付け削除。**紐付け project があればエラーで止める** | なし |

**`boid workspace configure <slug>` の詳細**:

- slug 必須。引数なしや `--all` は持たない (workspace は粒度ある単位、一括は意味不明)
- **スキャン対象**: 紐付け済み project 群 (DB の `project_workspaces` で workspace_id 紐付けされている全 project)。カレント project だけではない (= workspace スコープ横断)
- **マッチング**: project の `package.json` / `go.mod` / `task_behaviors[].hooks` のスクリプト中身を見て要求ツールを推定 → `boid kit list` のカタログから該当 kit を選ぶ → workspace.yaml の `kits:` に追加
- **足りない kit** が出たら、「project が `gh` を要求しているが github-cli kit がカタログに無い。マシンに gh を入れて `kit init` を再実行してください」と案内 (workspace configure では kit 生成しない、境界尊重)
- 既存 workspace.yaml がある状態: 対話エージェントが diff 提示 + 上書き確認 (`kit init` と同じ流儀)
- **project ごとに enable する kit を変える機構は持たない**: workspace 内に Go project と Node project が混在しても、両方の kit が workspace の `kits:` に乗る (workspace 単位の粒度を守る)
- sandbox プロファイル: init 専用 (後述)

**`boid workspace remove <slug>`**:

- 紐付け project が残っている状態では**エラーで止める** (`--force` は持たない、必要なら先に `clear` を叩く)

### `boid project ...`

| コマンド | 役割 | エージェント |
|---|---|---|
| `boid project init [dir] [--workspace <slug>]` | 新規プロジェクトの `.boid/project.yaml` 雛形生成 (task_behaviors / worktree のみ、kit は触らない) + daemon 登録 + (任意で workspace 紐付け) | なし (静的テンプレ) |
| `boid project add <dir> [--workspace <slug>]` | 既存プロジェクト (`.boid/project.yaml` がある) を daemon 登録 + (任意で workspace 紐付け) | なし |
| `boid project migrate <dir> [--workspace <slug>] [--apply]` | 旧スキーマの project.yaml を新スキーマに変換 (legacy kit + workspace.yaml に分解) | なし (静的変換) |

**廃止コマンド**:

- `boid init [dir]` (`cmd/init.go`) — 役割を `kit init` (kit カタログ) と `project init` (雛形) と `workspace configure` (環境マッチング) に分解
- `boid project local ...` (`cmd/project_local.go`) — `project.local.yaml` 廃止に伴い撤去 (内容は workspace.yaml へ集約、後述「マイグレーション」)

**`boid project init` が静的テンプレな理由**:

- portable な雛形 (task_behaviors の枠 + worktree / base_branch) はマシン依存ゼロで対話する内容が無い
- 「executor / supervisor の標準 behavior 雛形を埋め込むか」 はマシン関係なく boid 標準パターンなので静的テンプレに焼ける
- 最小は `executor` (readonly: false) 1 つ + `supervisor` (readonly: true) を選択肢として含めるが、対話エージェントは介在させない
- エージェント起動コスト省ける、シンプル

**`--workspace <slug>` flag セマンティクス** (`project init` / `project add` 共通):

- 指定時: project 登録と同時に `project_workspaces` 紐付けも書く
- 未知 slug 指定時: **get-or-create** (DB row だけ作る、workspace.yaml は `workspace configure` 待ち)
- 未指定時: 紐付けなし。あとから `boid workspace assign` で付けられる

### オンボーディング 3 段

| 順序 | コマンド | エージェント |
|---|---|---|
| 1 | `boid kit init` | あり |
| 2a | `boid project init [dir] [--workspace <slug>]` (新規 project) | なし |
| 2b | `boid project add <dir> [--workspace <slug>]` (既存 project) | なし |
| 3 | `boid workspace configure <slug>` | あり |

**シナリオ別の叩く回数**:

- 新規マシン + 新規 project: **1 → 2a → 3** (3 段全部)
- 新規マシン + 既存 project: **1 → 2b → 3**
- 既存マシン + 新規 project: **2a → 3** (kit init は飛ばす)
- 既存マシン + 既存 project: **2b → 3**
- 既存 workspace に project 追加だけ (要求変わらず): **2b のみ** (configure 不要)

**旧 `boid init` 起動時のガイダンス** (削除キー扱いと同じ流儀):

```
boid init は廃止されました。次の 3 コマンドで初期化してください:
  1) boid kit init               (このマシンの kit カタログ生成)
  2) boid project init [dir]     (新規プロジェクト雛形)
     boid project add <dir>      (既存プロジェクト登録)
  3) boid workspace configure <slug>   (workspace 設定生成)
詳細は docs/ja/guide/onboarding.md を参照
```

**オンボーディング docs**: 専用ラッパー (旧 `init env` のようなもの) は作らない (`boid <名詞> <動詞>` 体系に合う名詞がなく、3 コマンドで足りるため)。`docs/ja/guide/onboarding.md` に 3 段の流れと各シナリオを書く。

## init 専用 sandbox プロファイル

`boid kit init` と `boid workspace configure` が共通で使うプロファイル。「検出 (read) は自由・漏出 (write/exfil) を塞ぐ」設計。

### 設計

| 軸 | 仕様 |
|---|---|
| **read** | 全 FS (`/` を ro で rbind)。base dirs (`/bin` `/sbin` `/lib` `/lib64` `/usr` `/etc`) も `ReadOnly: true` で bind |
| **write** | 呼び出し側が allowlist 指定。`kit init` → `~/.local/share/boid/kits/` (親 dir RW bind)、`workspace configure` → `~/.config/boid/workspaces/<slug>.yaml` (host 側で pre-create してから RW bind、または親 dir RW bind) |
| **host_command** | 全無効 (`spec.HostCommands` 空) |
| **BuiltinPolicies** | **全無効** (Codex 第 2 ラウンドレビュー指摘 ①: `boid fetch` 経路の SSRF/exfil を塞ぐ。fetch builtin は host 側で HTTP GET するため sandbox の egress allowlist を経由しない) |
| **broker socket** | mount しない (host_command/builtin が無効なので不要) |
| **egress** | 通常 allowlist (HTTPS_PROXY、sandbox 内からの直接通信用) |
| **sandbox 内 exec** | 可能 (`which volta` / `ls ~/.volta/bin` 等の検出に必要) |

**read 全 FS の理由**: 開発ツール (volta / anyenv 系 / uv / nuget / go 等) が `$HOME` 以下に散在し、機密ディレクトリだけを除外するのが困難。read 範囲は絞らない。

**機密の exfil 防御** (出口を**三段**で考える):

1. **sandbox 内通信** = egress allowlist (HTTPS_PROXY) で遮断
2. **host 直実行** = host_command + BuiltinPolicies 全無効で遮断 (Codex ① 対応)
3. **model context** = read した内容はエージェントのコンテキストに乗り LLM プロバイダへ送られる。これはハーネス自身の API 接続で sandbox の egress proxy を通らないため、機構では遮断できない

(1)(2) は機構で塞げるが、(3) と残る間接経路 (read した機密を `kit.yaml` / `workspace.yaml` に書く) は**エージェント自身のチェック (self-policing) と secret-free 規約 + 後段 scan** に依存する。これらは悪意エージェントには効かないが、想定脅威 (信頼されたユーザの環境セットアップ中のうっかり混入) には十分とする。

### 実装

sandbox 層は既に task / command の model から独立している (`internal/sandbox` は `orchestrator` を import しない)。したがって**「密結合を解きほぐす」大リファクタは不要**で、`sandbox.Spec` の拡張のみで対応可能。

| 項目 | 現状 | 必要な変更 |
|---|---|---|
| Profile enum | なし | `Spec.Profile sandbox.Profile` 追加 (enum: `Default` / `Init`) |
| read 全 FS | base dirs のみマウント | `Profile == Init` で `BuildPlan` (`internal/sandbox/plan.go:25-37`) が host root を ro rbind する分岐 |
| base dirs ReadOnly | `/bin` `/usr` 等が `MountRBind` + `Slave` で bind されるが **`ReadOnly` フラグ無し** (現状 uid_map `0:1000:1` の DAC で抑制) | `Init` プロファイルで base mount に `ReadOnly: true` を追加 (主題とは独立だが同じプロファイル実装で同時に直す) |
| write allowlist | project 全体を `Visibility.Writable` フラグで一括 rw/ro | 既存の `IsFile` binding + `Mode:"rw"` を活用。**生成対象ファイルは bind 時点でまだ存在しない**ため、kit は親 dir RW bind、workspace.yaml は host 側に空ファイルを pre-create してから RW bind (create-then-bind) するか同様に親 dir を RW bind |
| host_command + builtin 無効 | `spec.HostCommands` 空でも `spec.BuiltinPolicies` 非空なら broker 登録される (`runner.go:180`) | `Profile == Init` なら HostCommands/BuiltinPolicies が空でなくても broker 登録しない (or spec を強制空に正規化) |
| broker socket mount | host_command/builtin あれば mount | `Profile == Init` なら mount しない |
| egress allowlist | 既存 (`ProxyPort` + `AllowedDomains`) | 再利用 |

task / command 側の変更は最小 (新プロファイルを選ぶ syntax のみ)。既存設計 (orchestrator → dispatcher 翻訳 → 中立 sandbox.Spec) を踏襲する。

### secret-free 規約 + 後段 scan

shell adapter 経路で kit dir 全体が task sandbox に bind される問題 (Codex 第 2 ラウンドレビュー指摘 ②: `sandbox_builder.go:227-234` で `Visibility.KitRoots` をディレクトリごと ReadOnly bind するため kit.yaml も読める) は、**kit.yaml に生 secret 値を書かない設計を厳守する**ことで自動的に無害化される。

kit.yaml に書かれるもの:

- `meta` — 公開情報
- `host_commands` — path / args / env、env の secret 注入は `secret:<key>` プレフィックスで**参照のみ**
- `additional_bindings` — マシン上のパス
- `env` — plain のみ (生 secret 値は書かない)

→ 生 secret 値は kit.yaml のどこにも書かれない。task sandbox から kit.yaml が読めても、漏れるのは:

- マシン上のパス情報 (タスクは元々マシン上で動いているため追加情報なし)
- secret キーの**参照名** (`secret:npm_token` 等、値ではない)

実 secret 値は DB に AES-GCM で暗号化されており、host_command 実行時に **broker 経由で env に注入**される。kit.yaml ファイル経由では出ない。

**ガードレール (後段 scan)**: 生成スキルの self-policing 補強として、`kit init` / `workspace configure` の生成直後に kit.yaml / workspace.yaml を **secret-like パターン scan** する後段チェックを置く:

- 正規表現で `[A-Za-z0-9_-]{32,}` の生文字列が値として現れていないか
- `password=` / `token=` / `secret=` 等のリテラル文字列が値部分に混入していないか
- ヒットしたら警告を出して書き込みを refuse (要 self-policing で書き直し)

**結論**: ディレクトリ構造変更も bind 粒度変更も不要。「kit.yaml に生 secret を書かない」 設計規約 + 生成後 scan で代替する。

## マイグレーション

同僚が既に boid を運用しているため、互換性を明示的に設計する。boid 既存の「削除キーは移行ガイダンス付きエラーにする」流儀 (`spec_loader.go` の `workspace_id` / `hooks` / `gates` 拒否) を踏襲する。

### ハードカットオーバー (削除キー化)

`ReadProjectMeta` (`spec_loader.go:79`) で削除キーを検出したらエラー + 移行ガイダンス。対象:

- top-level: `kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`
- behavior-level: `task_behaviors.<name>.kits` (2 階層、behavior 粒度の kit 指定機能廃止)

エラーメッセージ例:

```
project.yaml: top-level "host_commands" is no longer supported.
Migration:
  1) Run: boid project migrate <dir>           (dry-run)
  2) Confirm the plan, then re-run with --apply
See docs/ja/guide/migration.md for details.
```

### `boid project migrate <dir>` の動作

**重要 (実装経路)**: migrate は **daemon 起動が拒否されている状態 (削除キー検出エラーで daemon が立たない) からの救済経路**として動作する必要があるため、**daemon を経由せず CLI が yaml ファイルと DB を直接読み書きする**。daemon の起動状態に依存しない (API 経路は使わない)。DB アクセスは `internal/db` を CLI から直に open して `project_workspaces` テーブルと secret テーブルを touch する。

| フェーズ | 動作 |
|---|---|
| 1. legacy 読み込み | `readProjectMetaLegacy` (新規) が raw map 経由で旧スキーマを丸ごと吸い上げる。削除キーをエラーにしない (`ReadProjectMeta` とは別経路) |
| 2. workspace 解決 | `--workspace` flag → 既存 `project_workspaces` 紐付け (DB 直読み) → インタラクティブに選択、の順で workspace slug を決定 |
| 3. 変換プラン提示 | dry-run で「こんな yaml を生成します + 旧 namespace `<old>` の N 件の secret を `<workspace_id>` namespace にコピーします」プランを表示 |
| 4. `--apply` 時のみ実行 | kit.yaml 生成 + workspace.yaml 更新 + project.yaml から削除キーを物理削除 + **secret の copy migrate** (旧 namespace の全 key を新 namespace に複製、旧 namespace は残す) |

**変換ルール**:

| 旧 project.yaml フィールド | 新しい所在 |
|---|---|
| `kits` (kit ref リスト) | workspace.yaml `kits:` に移植 (ref 形式変更 = `<name>` 単体への正規化込み) |
| `env` | workspace.yaml `env:` にマージ |
| `host_commands` + `additional_bindings` | `~/.local/share/boid/kits/legacy-<project_id>/kit.yaml` に**まとめて移す** (kit 分割の自動推定はリスキー、legacy 一括が安全) |
| `capabilities.docker` | workspace.yaml `capabilities.docker` |
| `secret_namespace` | 旧 namespace の全 secret を新しい **workspace_id namespace に copy migrate**。旧 namespace は rollback 用に残す (`--apply` で削除はしない、別途 `boid secret cleanup` 等で後始末)。**collision policy** は後述「secret migration の collision policy」 節を参照 (default refuse、別 project の secret 破壊を防ぐ) |
| behavior-level `task_behaviors.<name>.kits` | workspace 共通 kits との重複検出 + 残りを workspace.yaml に追加 |

**dry-run デフォルト**: `--apply` 無指定なら変換プランを yaml で出力して終わる (適用しない)。ユーザが確認してから `--apply` を叩く流れ。

**secret migration の collision policy** (Codex 第 4 ラウンド指摘 3 対応):

複数 project が同一 workspace を共有する場合、別 project が既に新 namespace = workspace_id に同名 key を書き込んでいる可能性がある。現行の `internal/dispatcher/secret_store.go:71` は `ON CONFLICT(namespace, key) DO UPDATE` で**サイレント上書き**するため、無防備にコピーすると別 project の secret が破壊される。

これを防ぐため、migrate は以下のフローで動作する:

1. **dry-run で collision を列挙**: 「`<old_ns>:npm_token` を `<workspace_id>:npm_token` に複製予定だが、後者には既に値が存在する。値の hash が一致 / 不一致を表示」
2. **`--apply` 時のデフォルトは refuse**: collision が 1 件でもあれば migrate 全体を停止し、collision リストと flag ガイダンスを表示
3. **明示 flag で挙動を選択**:
   - `--on-collision skip`: collision している key だけスキップして他をコピー (推奨)
   - `--on-collision overwrite`: 明示的に上書きを許可 (危険、確認プロンプト付き)

「黙って壊さない」 を原則とし、明示的なユーザ意思表示なしには別 project の secret を上書きしない。

**エージェント呼び出しなし**: 静的変換で実装する。「host_commands を 1 個の legacy kit に押し込む vs 適切に kit 分割する」 判断は静的だと荒いが、最初は legacy 一括で割り切る。「適切な分割は移行後に `boid kit init` を再実行してマシン環境ベースで作り直してください。legacy kit は migration の足場です」とガイダンスする。

### `project.local.yaml` の廃止

旧 `project.local.yaml` が担っていた env / host_commands / additional_bindings / secret_namespace のローカルオーバーライドは、すべて workspace.yaml が引き継ぐ (granularity が project 単位から workspace 単位に格上げ)。CLI 側も `boid project local ...` (`cmd/project_local.go`) を撤去する。`project migrate` が同時に吸い上げる対象に含める。

### 同僚への影響と手順

- アップグレード後、既存 project.yaml が削除キー検出でエラーになり、daemon 起動を拒否 (黙って壊れるのではなく明示的に止まる)
- 救済経路: `boid project migrate <dir>` で plan 確認 → `--apply` で実行
- 救済後の project.yaml は git diff で「環境フィールドが消える」 形になる (= 狙い通り、環境固有情報が剥がれる)

## 退化点と意図的トレードオフ

### 環境境界 = secret 境界 = peer 境界の三者一致

workspace は **3 つの境界を律する** ことになる:

1. **環境境界** — kit / env / capabilities の有効範囲
2. **secret 境界** — namespace = workspace_id で固定
3. **peer 境界** — `resolveWorkspacePeers` (`internal/dispatcher/runner.go:342`) による project 間 ro 可視性

旧モデルでは secret namespace と workspace が独立だったので「peer は共有したいが secret は分けたい」が表現できたが、新モデルでは表現できなくなる。これは**素直さを優先した意図的な退化**。

退化が問題になる場面では「workspace を分けて secret 境界も分ける」が正攻法。粒度は粗くなるが「環境を共有する project は資格情報も共有する」というモデルを採る。

### capability provides / requires の不在

project は「node が要る」 を宣言しない (capability の provides / requires 抽象を導入しないため)。よって `workspace configure` の生成スキルは **project の repo 中身と task_behavior の hook を読んで必要ツールを推測する**ヒューリスティックで決める。

例:

- `package.json` / `pnpm-lock.yaml` → node
- `go.mod` → go
- `*.csproj` → dotnet
- hook script 内の `gh` / `docker` 呼び出し → github-cli / docker kit

検出は網羅性を保証しない (ヒューリスティック)。漏れた要求は runtime で顕在化する (事前検出はスコープ外 C)。生成スキルは差分追加なので、漏れても後から再実行して補える。

### degraded 窓 (workspace.yaml 未生成)

`project add --workspace` から `workspace configure` までの窓で task の hook が走ると、workspace が供給する kit / env が無い状態になる。shell adapter (hook script 実行) は `Visibility.KitRoots` が空だと kit 由来の binding を bind せず、hook script が sandbox に見えずに**処理が失敗しうる** (`sandbox_builder.go:223-234`、PR #594 と同じ failure mode)。

この窓を「workspace.yaml の存在チェックで事前に止める」設計は**採らない**。存在だけ確認しても workspace の中身が不十分なら結局 runtime で失敗するので中途半端であり、かつ本設計の方針 (要求と供給のミスマッチは runtime で顕在化させる) とも一貫しない。

よってこの窓では **hook は起動するが、kit / env 不足で処理が失敗しうる (degrade を許容し runtime で顕在化させる)**。失敗時に原因を辿れるよう、エラーに `workspace configure <slug>` の案内を添える。

## 実装メモ

### workspace runtime → JobSpec 配線 (load 経路 2 段化)

現行は `meta.Capabilities.Docker` を `internal/orchestrator/planner.go:96` と `cmd/exec.go:88` が直接見ている (`meta.Capabilities.Docker != nil` で docker proxy を on にする経路)。新モデルでは docker capability / kits / env が **workspace.yaml に移動**するため、これらを `meta` 経由の見え方に保ったまま **load 経路を 2 段に分けて** inject する。下流参照は無変更。

**load 経路の 2 段化** (Codex 第 4 ラウンド指摘 2 対応):

現行の `ReadProjectMetaWithKits(workDir, resolver)` は workDir と KitResolver しか受け取らず、DB も workspaceID も持っていない (`internal/orchestrator/spec_loader.go`)。さらに project 登録時 (`internal/api/project_service.go:31`) は **project row 作成前** に meta load するため、その時点では workspaceID 自体が未確定。loader signature に workspaceID/WorkspaceStore を足すと「workspaceID 未確定の場面」 と「workspaceID 引きたい場面」 を 1 関数で混ぜることになり、責務が濁る。

そこで load 経路を以下の 2 段に分ける:

| 段 | 経路 | 用途 |
|---|---|---|
| **生 load** | `ReadProjectMetaWithKits(workDir, resolver)` — **現行 signature 不変** | project 登録時の meta validate / daemon 起動時の syntactic check |
| **hydrate load** | `ProjectRepository.GetWithWorkspace(projectID)` (**新設**) — project row → workspaceID → WorkspaceStore.Load → meta + workspace を merge して返す | dispatcher が job spec を build するとき (`internal/orchestrator/planner.go` 入口) |

現状 `internal/orchestrator/project_store.go:25` 経由で `ReadProjectMetaWithKits` を呼んでいる dispatcher / planner 系 caller は新設の `GetWithWorkspace` を呼ぶように差し替える。loader の責務は project.yaml の parse + kit merge までで止まり、**workspace inject は ProjectRepository 層に集約**される。

**inject 内容** (hydrate load 時):

- workspace.yaml の `capabilities.docker` → `meta.Capabilities.Docker` に inject (前述の `yaml:"-"` runtime-only field)
- workspace.yaml の `kits[]` を解決 → 既存の `MergeKitRuntime` / `MergeKitMetaIntoBehavior` を再利用して `meta.TaskBehaviors[*].HostCommands` / `Env` / `AdditionalBindings` / `KitRoots` に merge
- workspace.yaml の `env` → `meta.Env` に merge (kit が先、workspace が後勝ち、host_commands は error reject — 前述「kits の順序と衝突」 ルールに従う)
- **`project.WorkspaceID` → `meta.SecretNamespace` に inject** (前述の `yaml:"-"` runtime-only field、Codex 第 5 ラウンド指摘 1 対応)。これにより `planner.go:104` の `SecretNamespace: meta.SecretNamespace` 配線がそのまま機能し、`runner.go:194` で workspaceID namespace の secret が解決される。**degraded 窓 (workspace.yaml が not found だが workspaceID は DB に存在する状態) でも SecretNamespace の inject は実行する** — Capabilities / kits / env は workspace.yaml が無いため空のまま続行するが、secret namespace は workspaceID から一意に決まるので有効化する意味がある (Codex 第 6 ラウンド指摘 1 対応)。workspaceID 自体が空 (project が workspace に未紐付け) のときだけ `runner.go:195-197` のフォールバックで `"default"` namespace に落ちる

これにより `planner.go:96` / `cmd/exec.go:88` の参照箇所は **無変更** で動く (これらは ProjectRepository から得た hydrated meta を受け取る)。docker capability が silently disabled になる事故を防ぐ。

**degraded 窓との関係**: workspace.yaml が未生成な状態 (`project add --workspace foo` 直後で `configure` 前) では `GetWithWorkspace` 内で `WorkspaceStore.Load` が `not found` を返す。この場合は warning ログ + Capabilities = zero value で続行 (= 後述「degraded 窓」 節と一貫した挙動)。daemon は起動拒否せず、hook 実行時に kit / env 不足で job が runtime fail する。

**生 load の用途**: project 登録時に「meta が syntactic に valid か」 だけ確認したい場面では生 load を使う。workspaceID 未確定でも問題なく動く (workspace inject は登録後の hydrate に任せる)。

### startup 経路を warn 継続 → error return に変更 (Codex 第 5 ラウンド指摘 2 対応)

本再編は「workspace.yaml の parse 失敗 = daemon 起動拒否」 / 「project.yaml の削除キー検出 = daemon 起動拒否」 をハードカットオーバーで謳っているが、現行の `internal/server/wire.go:46-62` の `buildProjectStore` は **parse 失敗を `slog.Warn` で続行**してしまう (`store.LoadAll(projects)` の戻り値 errors を warn しているだけ)。このままでは「拒否すると言ったのに warn 継続でサイレントに壊れた project が daemon 起動を許す」 退化が起きる。

**実装での変更**:

- `buildProjectStore` の `LoadAll` エラーハンドリングを `slog.Warn` 続行から **`return nil, fmt.Errorf(...)` に変更**。 1 件でも parse error が出たら daemon 起動を拒否する
- 同様に workspace.yaml の parse error 経路 (新設 `WorkspaceStore.Load` の error) も startup 時に検出して error return する
- エラーメッセージには該当 yaml の path と「`boid project migrate <dir>`」 または「`boid workspace configure <slug>`」 の救済コマンドを添える

**同僚への影響**: 既に parse error 状態の project / workspace を持つマシンでは daemon が起動できなくなる。これは意図的な fail-fast 挙動で、本再編の「削除キー検出時に明示的に止まる」 方針と一貫。救済経路として `boid project migrate <dir>` は **daemon 非経由 CLI 直接経路** で実装する (前述「`boid project migrate <dir>` の動作」 節) ため、daemon が拒否状態でも migrate を回せる。

### 埋込スキル bind (Phase 3-e で完了済み)

追加作業不要。

- 埋込スキル (`/boid-task` / `boid-orchestrate` / `boid-web`) は `internal/skills/deploy.go:12` で embed され、`internal/server/server.go:68` の `DeployAll` が `~/.local/share/boid/skills/<name>` に展開する
- 各 adapter の `Bindings()` (`internal/adapters/claude/bindings.go:24` 他) が `~/.local/share/boid/skills/<name>` → `~/.claude/skills/<name>` を返す
- boid-kits への実装依存はゼロ

**申し送り**: shell adapter (hook script 実行) だけは `Bindings()` が nil のため legacy kit binding 経路 (`internal/dispatcher/sandbox_builder.go:150-159` の `expandedBindings` + `Visibility.KitRoots`) に乗っている。kit が workspace 配下に移行する際、この経路が新しい供給元 (workspace が有効化した kit の host_commands / additional_bindings) を正しく受け取るよう配線を確認すること。落とすと hook script が sandbox に見えず task が死ぬ (PR #594 退行と同じ failure mode)。

### hook script の所在

task_behavior の hook は project に残る。ScriptPath を持つ hook (builtin / script) の実体配置先を確認する (project の `.boid/` 配下か)。agent hook は ScriptPath 不要 (Phase 3-e の fallback 合成)。

### `boid kit init` 生成スキル

スキル設計は別途詰めるが、骨子:

- **入力**: マシンの環境 (PATH 内の binary、`$HOME` 以下の標準的な dir 構造)
- **出力**: `~/.local/share/boid/kits/<name>/kit.yaml` 群
- **リファレンス**: 旧 boid-kits の go-dev / github-cli / volta / node / docker 等の kit 雛形を **embedded スキルテキスト** として持つ
- **検出ヒューリスティック例**:
  - `volta` バイナリ in PATH → `node` kit (volta 経由のパス埋め込み)
  - `nvm` のみ → `node` kit (nvm 経由)
  - system `node` のみ → `node` kit (system PATH)
  - `gh` in PATH → `github-cli` kit
  - `docker` socket 検出 → `docker` kit
  - `go` in PATH → `go` kit
- **衝突時の対話**: 既存 kit があれば diff 提示、上書き or skip を user 選択

### `boid workspace configure` 生成スキル

- **入力**: workspace に紐付いた project 群の repo 中身 (`package.json` / `go.mod` / `task_behaviors[].hooks` のスクリプト)
- **カタログ**: `~/.local/share/boid/kits/*/kit.yaml` を read
- **出力**: `~/.config/boid/workspaces/<slug>.yaml`
- **マッチング**: project シグナル → kit name の対応表 (スキルテキスト内に hardcode):
  - `package.json` 存在 → `node` kit
  - `go.mod` 存在 → `go` kit
  - hook 内 `gh ` 呼び出し → `github-cli` kit
- **足りない kit**: 「kit カタログに該当が無い、`boid kit init` 再実行 or 手動で kit.yaml 追加」をガイダンス出力

## 将来のリファクタ展望: ランタイム型と YAML 型のギャップ

本再編は **YAML 層** での責務分離 (project / workspace / kit の 3 軸) を主眼にしており、**ランタイム層**の Go 型構造は意図的にそのまま残す。型整理は本再編の射程外として、将来別議題で取り組む対象とする。

### 隠れた副次効果: project は b 専属、workspace + kit は a 専属になる

ランタイム (orchestrator / dispatcher / sandbox) が必要とする情報は実は 2 軸だけ:

- **a. SandboxSpec** (仮称): binding / host_commands / env / capabilities (= sandbox 構築と実行に必要なもの)
- **b. TaskContext** (仮称): task_behaviors (hooks / traits / default_instruction) / worktree / base_branch / fork_point / default_task_behavior (= task コンテキスト構築に必要なもの)

本再編が終わると、各 YAML はランタイム軸に綺麗に対応する:

| YAML | → a (SandboxSpec) | → b (TaskContext) |
|---|---|---|
| project.yaml | (本再編で除去) | ✅ |
| workspace.yaml | ✅ | — |
| kit.yaml | ✅ | — |

つまり「project / workspace / kit」 は YAML 単位の概念で、ランタイム時点では a と b に**解体**されている。本再編後は project が b 専属、workspace + kit が a 専属に分かれる — これは「project = portable な作業パターン」「workspace + kit = machine-local なツール供給」 という責務分離の自然な帰結。

### 現状の型構造の残骸 (= 兼用 struct パターン)

ところが現行 boid の Go 型は a + b の境界がランタイム側にも入り込んでいる:

```go
type ProjectMeta struct {
    // b 責務 (本再編後の project.yaml が持つもの)
    ID, Name, Worktree, BaseBranch, ForkPoint, DefaultTaskBehavior
    TaskBehaviors

    // a 責務 (本再編で yaml:"-" の runtime-only field 化、workspace から inject される容れ物)
    Capabilities `yaml:"-"`
}

type TaskBehavior struct {
    // b 責務 (本再編後の task_behaviors が持つもの)
    Readonly, Traits, DefaultInstruction
    Hooks `yaml:"-"`

    // a 責務 (kit / workspace から merge される runtime-only)
    Env, HostCommands, AdditionalBindings, KitRoots `yaml:"-"`
}
```

`ProjectMeta` も `TaskBehavior` も a + b 両方を**兼用**している。今回はこれを維持するが、理想形は a / b を別 struct に分けることだ:

```go
type SandboxSpec struct { /* a 専属: binding / host_commands / env / capabilities */ }
type TaskContext struct { /* b 専属: behaviors / worktree / base_branch / ... */ }
```

ランタイムは `SandboxSpec` + `TaskContext` だけを受け取り、`ProjectMeta` / `WorkspaceMeta` / `KitMeta` という YAML 単位はランタイムから消える。

### このリファクタを今やらない理由

- **動作上は不要**: 現状の兼用 struct でも orchestrator / dispatcher / sandbox の責務分解は崩れていない
- **既存パターンと一貫**: `TaskBehavior` の `yaml:"-"` 群が既に「兼用」 パターンを採用しており、本再編で `Capabilities` も同じ流儀に揃える方が一貫性が高い
- **触る範囲が広い**: planner / dispatcher / cmd の全参照を SandboxSpec/TaskContext 経由に書き換える機械的作業になり、本再編の責務範囲を超える

本再編は将来の型整理の **準備段階** という位置づけ。YAML 層で責務が分かれてさえいれば、後の型リファクタは「表面の参照を全部書き換える」 機械的作業に落とせる。

## スコープ外 (別テーマ)

- **C: セッション診断 / マルチエージェント構成の評価・最適化**。capability 要求と供給のミスマッチの事前検出を含む、エージェント実行中の問題の観測・診断・最適化。プロジェクト構成とセッション診断は独立した大きなテーマとして別途取り組む
- **E: workspace 単位の egress allowlist**。便利だが proxy の管理が複雑化する懸念があり、見当がついてから検討

## オープンクエスチョン

- ScriptPath を持つ hook の実体配置先 (project の `.boid/` 配下か)
- shell adapter 経路 (`sandbox_builder.go:150-159`) の legacy kit binding を、workspace が有効化した kit の供給に繋ぐ具体配線
- secret-like パターン scan の判定基準 (false positive と感度のバランス、正規表現の具体)
- `boid kit init` 生成スキル + `boid workspace configure` 生成スキルの実装フォーマット (SKILL.md / scripts / embed 戦略)
- `boid project migrate` の dry-run 出力形式 (yaml diff か、コマンド列か)

---

## 改訂履歴

- **第 1 ラウンド** (2026-06-23) — 設計プラン初稿
- **第 2 ラウンド** (2026-06-24) — Codex レビュー反映 (決定 A/B/C/D + ①②③④ の番号付け)
- **第 3 ラウンド** (2026-06-25) — Codex 第 2 ラウンドレビュー + 方針再確認を反映:
  - kit を 「専有」 から**グローバル共有**に戻す (旧プランで誤って 「専有」 にしていたのを訂正)
  - kit と workspace のコマンド体系を**分離維持** (旧プランで `workspace configure` に一本化しようとしていたのを撤回)
  - 責務を明確化: `boid kit init` = 環境スキャン (マシン状態のインベントリ) / `boid workspace configure` = project マッチング
  - workspace.yaml から secret 注入宣言を削除 (kit 側で完結、workspace は kit を参照するだけ)
  - kit ref を `<name>` 単体に simplify (`local/` prefix 撤去、kit は local オンリーなので prefix が冗長)
  - kit.yaml から `task_behaviors` / `hooks` / `detect` / `requires` / `provides_agent` / `deprecated` を全て削除 (責務明確化、生成スキル複雑化を避ける)
  - `BehaviorSpec` 型ごと削除 (kit が behavior を provide する経路を廃止)
  - init 専用 sandbox で `BuiltinPolicies` も全無効化 (Codex ① 対応: `boid fetch` 経路の SSRF/exfil を塞ぐ)
  - shell adapter kit.yaml 露出は secret-free 規約 + 後段 scan で対応 (ディレクトリ変更なし、Codex ② 対応)
  - workspace slug 検証を 3 層多層防衛 (Codex ⑥ 対応)
  - `boid project migrate` 新設 (Codex ⑤ 対応、legacy loader 経路 + 静的変換 + dry-run デフォルト)
  - 決定 A/B/C/D + ①②③④ の番号参照を撤去 (本文に inline 化)
- **第 4 ラウンド** (2026-06-25) — Codex 第 3 ラウンドレビュー反映:
  - `default_task_behavior` を project.yaml 残置フィールドに明示 (落とし指摘 1 対応)
  - `BehaviorSpec` 型撤去を**撤回**。behavior は task 作成時に決まる動的概念で、project の `task_behaviors:` は名前付き雛形 (プリセット) を提供するにすぎないと明文化。inline behavior 指定 API (`CreateTaskRequest.behavior_spec`) は kit 経路廃止と独立に存続 (指摘 2、方針 A 採用)
  - secret_namespace は migrate 時に旧 namespace → workspace_id namespace へ **copy migrate**、旧 namespace は rollback 用に残す (指摘 3 対応、既存 secret 失効を防ぐ)
  - workspace.yaml SoT 設計を追加: slug = yaml 名 = DB workspace_id の三者一致、yaml と DB の片方欠落許容、権威の分担 (kit/env/cap → yaml、紐付け → DB)、`WorkspaceStore` 責務 (atomic tmp+rename write、parse error 時は daemon 起動拒否) (指摘 4 対応)
  - workspace runtime → JobSpec 配線を **`ReadProjectMetaWithKits` で inject** する設計を明文化、`planner.go:96` / `cmd/exec.go:88` の参照箇所は無変更。docker silently disabled 事故防止 (指摘 5 対応)
  - host_commands 衝突ルールを「override」 から現行どおり「**error reject**」 (`MergeKitRuntime` `spec_loader.go:49-64`) に訂正。env と additional_bindings は後勝ち / union だが host_commands は重複拒否 (指摘 6 対応)
  - `boid project migrate` は daemon 起動拒否状態からの救済経路として、**daemon 非経由の CLI 直接経路** (yaml ファイル + DB 直接 read/write) で実装と明示 (open question 対応)
- **第 5 ラウンド** (2026-06-25) — Codex 第 4 ラウンドレビュー反映:
  - `Capabilities` / `DockerCapability` struct の削除を**撤回**。`ProjectMeta.Capabilities` を `yaml:"-"` の runtime-only field 化し、workspace.yaml から hydrate 時に inject される容れ物として使う。下流 (`planner.go:96` / `cmd/exec.go:88`) の参照は無変更で済む。`TaskBehavior` の merged field 群と同じ「兼用 struct」 パターンに揃える (指摘 1 対応)
  - workspace inject の経路を **load 経路 2 段化** に修正。`ReadProjectMetaWithKits` の signature は不変、新設 `ProjectRepository.GetWithWorkspace(projectID)` で hydrate。project 登録時 (workspaceID 未確定) と dispatch 時 (workspaceID 確定) を経路で分ける (指摘 2 対応)
  - secret copy migration に **collision policy** を追加: dry-run で衝突列挙、デフォルト refuse、`--on-collision skip|overwrite` flag。複数 project が同一 workspace を共有するときに別 project の secret 破壊を防ぐ (指摘 3 対応)
  - `boid workspace list` を yaml + DB の slug **union** にし、各 slug の状態を `ready` / `unconfigured` / `empty` で表示。`workspace show <slug>` は片方欠落時にその旨を明示。degraded workspace / 空 workspace が UI から消える事故を防ぐ (指摘 4 対応)
  - 「将来のリファクタ展望」 節を新設。ランタイムが本当に必要なのは `SandboxSpec` (a) + `TaskContext` (b) の 2 軸で、project / workspace / kit は YAML 単位にすぎない。本再編後に project は b 専属、workspace + kit は a 専属に分かれる隠れた副次効果を明文化。型整理は別議題として保留
- **第 6 ラウンド** (2026-06-25) — Codex 第 5 ラウンドレビュー反映:
  - `ProjectMeta.SecretNamespace` を `yaml:"-"` runtime-only field に転用、hydrate 時に `project.WorkspaceID` を inject。yaml 入力経路は削除キー化で塞ぐが struct フィールドは温存し、`planner.go:104` / `runner.go:194` の secret 解決配線をそのまま動かす。これが無いと secret が全て `"default"` namespace にフォールバックする退化が起きる (Codex 第 5 ラウンド指摘 1 対応)
  - 実装メモに **startup 経路の挙動変更**を明記: 現行 `internal/server/wire.go:46-62` の `buildProjectStore` は parse error を warn 継続するが、本再編では `error return` に変えて daemon 起動を fail-fast 拒否する。workspace parse error も同様に拒否。救済は daemon 非経由の `boid project migrate` 経路 (Codex 第 5 ラウンド指摘 2 対応)
- **第 7 ラウンド** (2026-06-25) — Codex 第 6 ラウンドレビュー反映 (軽微な整合性修正):
  - degraded 窓の説明を修正。`workspace.yaml not found` の状態でも workspaceID は DB に存在するため、`meta.SecretNamespace = project.WorkspaceID` の inject は実行する旨を明記。Capabilities/kits/env は空のまま続行するが secret namespace は workspaceID から一意に決まるので有効化する (Codex 第 6 ラウンド指摘 1 対応)
  - 「将来のリファクタ展望」 節の `TaskBehavior` コード例から **`Kits` フィールドを削除** (本再編で削除対象なので例から消して混乱回避、Codex 第 6 ラウンド指摘 2 対応)
