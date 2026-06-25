# kit / workspace / project 構成の再編

ステータス: ドラフト (第 3 ラウンド改訂 — 責務明確化 + kit グローバル化 / 2026-06-25)

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

worktree / base_branch / fork_point は VCS 構造の話で移植可能なため project 残留。task_behaviors が持てるのは readonly / traits / default_instruction / hooks のみ。

**削除キー化するフィールド** (検出 → ガイダンス付きエラー):

- top-level: `kits`, `env`, `host_commands`, `additional_bindings`, `secret_namespace`, `capabilities` (docker 含む)
- behavior-level: `task_behaviors.<name>.kits` (behavior 粒度の kit 指定機能廃止 — 「この behavior は volta で」は環境判断を behavior に焼く行為で本再編が消す対象)

**現行 Go 構造体から削除する型 / フィールド** (`spec_types.go`):

- `ProjectMeta.Kits []KitRef` (line 406)
- `TaskBehavior.Kits []KitRef` (line 355)
- `Capabilities` struct 全体 (line 397-401) — docker capability は workspace.yaml へ移譲
- `DockerCapability` struct (line 391-394)
- `KitRef.Alias` フィールド (line 379) — workspace.yaml の `kits:` は string[] で alias 廃止
- `BehaviorSpec` struct (line 371-375) — kit が behavior を provide する経路を廃止したため不要

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

- **env merge ルール**: kit env が先に積まれ、workspace env が後で **override**
- **kits の順序**: 列挙順 = bind 順 (PATH 含む)。後ろの kit が前を override (現行 `project.yaml` の `kits` と同じセマンティクス)
- **slug がファイル名**: ファイル名 = slug。DB の `project_workspaces.workspace_id` と一致させる

**slug 検証** (path component / URL に乗るため必須):

- 許可: `[a-z0-9-]+`
- 拒否: 空文字 / `/` / `..` / 空白 / 大文字 / `_`
- 長さ: 1 〜 64 文字
- 共通バリデータ `internal/orchestrator/workspace_slug.go` の `ValidWorkspaceSlug(s string) error` を**3 層で共用**:
  - **CLI 入口** (`cmd/workspace.go` の `assign`, `cmd/project.go` の `add --workspace`) — 早期エラー、UX 改善
  - **API 入口** (`internal/api/project.go:SetWorkspace`, `project_service.go:SetProjectWorkspace`) — 400 Bad Request を早く返す
  - **Domain 層** (`internal/orchestrator/project_catalog.go:SetProjectWorkspace`) — **最終防衛** (DB INSERT 直前、ここを通らない経路は無い)

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
| `boid workspace list` | 一覧 (slug + 紐付け project 数) | なし |
| `boid workspace show <slug>` | workspace.yaml 内容 + 紐付け project リスト表示 | なし |
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

| フェーズ | 動作 |
|---|---|
| 1. legacy 読み込み | `readProjectMetaLegacy` (新規) が raw map 経由で旧スキーマを丸ごと吸い上げる。削除キーをエラーにしない (`ReadProjectMeta` とは別経路) |
| 2. workspace 解決 | `--workspace` flag → 既存 `project_workspaces` 紐付け → インタラクティブに選択、の順で workspace slug を決定 |
| 3. 変換プラン提示 | dry-run で「こんな yaml を生成します」プランを表示 |
| 4. `--apply` 時のみ実行 | kit.yaml 生成 + workspace.yaml 更新 + project.yaml から削除キーを物理削除 |

**変換ルール**:

| 旧 project.yaml フィールド | 新しい所在 |
|---|---|
| `kits` (kit ref リスト) | workspace.yaml `kits:` に移植 (ref 形式変更 = `<name>` 単体への正規化込み) |
| `env` | workspace.yaml `env:` にマージ |
| `host_commands` + `additional_bindings` | `~/.local/share/boid/kits/legacy-<project_id>/kit.yaml` に**まとめて移す** (kit 分割の自動推定はリスキー、legacy 一括が安全) |
| `capabilities.docker` | workspace.yaml `capabilities.docker` |
| `secret_namespace` | **捨てる** (workspace_id 固定なので不要) |
| behavior-level `task_behaviors.<name>.kits` | workspace 共通 kits との重複検出 + 残りを workspace.yaml に追加 |

**dry-run デフォルト**: `--apply` 無指定なら変換プランを yaml で出力して終わる (適用しない)。ユーザが確認してから `--apply` を叩く流れ。

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
