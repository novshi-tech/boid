# 生成スキル本体: `boid kit init` / `boid workspace configure`

> Status (2026-06-26 初稿):
> - 前提 plan: `docs/plans/kit-workspace-project-reorg.md` (本再編 8 PR + fix + 宿題 2 件は landed、 main = e3b1acc)
> - secret scan pkg は先行 PR で landed 想定 (本 plan は組み込み口の設計を含む)
> - 本 plan は「対話エージェント駆動の生成スキル本体」 を実装する PR シリーズを設計する

## 背景

再編 PR シリーズ (#631-#641) で **CLI 骨格**・**サンドボックスプロファイル (`sandbox.ProfileInit`)**・**broker 登録スキップ防衛**・**load 経路 2 段化** は landed 済。 残るのは「生成スキル本体」 そのもの:

| 入口 | 役割 | 現状 |
|---|---|---|
| `boid kit init` (`cmd/kit.go:26-34`) | 環境スキャン → `~/.local/share/boid/kits/<name>/kit.yaml` 群生成 | stub。 「未実装です」 メッセージのみ |
| `boid workspace configure <slug>` (`cmd/workspace.go:317-357`) | workspace 配下 project スキャン → kit カタログマッチング → `~/.config/boid/workspaces/<slug>.yaml` 生成 | スケルトン (空 `WorkspaceMeta{}` save のみ) |

つまり**サンドボックス器は鋳上がってる**が、 中で動かす agent prompt + skill 一式が無い。 設計プラン `kit-workspace-project-reorg.md:578-603` の骨子を実装可能なところまで詰めるのが本 plan の射程。

## オープンクエスチョン (前 plan からの引き継ぎ + 本 plan で答える)

`kit-workspace-project-reorg.md:673-679` で残されていた 4 件のうち 3 件は本 plan で答える。 残り 1 件 (ScriptPath 配置先) は task hook 経路の話で本 plan のスコープ外。

| # | 質問 | 答え (本 plan 案) |
|---|---|---|
| Q1 | `boid kit init` / `boid workspace configure` 生成スキルの実装フォーマット (SKILL.md / scripts / embed 戦略) | **SKILL.md 1 本 + 別ディレクトリの kit.yaml.tmpl 群**。 詳細 §3 |
| Q2 | shell adapter 経路の legacy kit binding を workspace 有効化 kit 供給に繋ぐ配線 | **本 plan スコープ外**: shell adapter は project hooks 経路に移行済 (`sandbox_builder.go:223-234` の `Visibility.KitRoots` は workspace hydrate 経由で既に動く)。 ProfileInit は kit/host_command 不要なので shell adapter 配線は触らない |
| Q3 | secret-like パターン scan の判定基準 | **PR1 (#642) で確定済**。 高エントロピー `[A-Za-z0-9_-]{32,}` + 5 つのリテラルマーカー + 4 種ホワイトリスト |

新規に決める必要がある事項:

| # | 質問 | 案 |
|---|---|---|
| Q4 | 生成スキル内で起動する harness の選択 (project 概念無し場面) | `default_harness` 設定 + 環境変数 override。 詳細 §4 |
| Q5 | 生成スキルが書く yaml の write 経路 (sandbox 内 → ホスト) | **親 dir RW bind + sandbox 内直接 write**。 詳細 §5 |
| Q6 | 既存 yaml との衝突対話 (上書き / merge / skip) | **エージェントが diff を host に表示しユーザに聞く** (CLI は素通しの interactive harness セッション) |

## 設計

### 1. 全体アーキテクチャ

```
┌──────────────────────────────────────────────────────────────┐
│ boid kit init       │ (CLI)                                  │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ skills.DeployAll(defaultSkillsDir())                   │  │
│  │   - daemon 未起動でも skill を host 展開 (冪等)        │  │
│  │   - 第 1 ラウンド指摘 1 対応                            │  │
│  └─────────────────────┬──────────────────────────────────┘  │
│                        │                                     │
│                        ▼                                     │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ BuildInitJobSpec (dispatcher 新関数)                   │  │
│  │   - SandboxProfile = ProfileInit                       │  │
│  │   - Argv = [harness binary, ...skill 起動 prompt]      │  │
│  │   - Writable = [~/.local/share/boid/kits]              │  │
│  │   - Bindings = 通常 skills (展開済を bind)              │  │
│  └─────────────────────┬──────────────────────────────────┘  │
│                        │                                     │
│                        ▼                                     │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ sandbox 起動 (既存 dispatcher.RunOuter)                │  │
│  │   - host root を ro rbind (ProfileInit 分岐)           │  │
│  │   - 親 dir RW bind で書き込み許可                       │  │
│  │   - broker socket mount しない                          │  │
│  │   - ServerSocket: kit init は空 / configure は         │  │
│  │     client.DefaultSocketPath() (§2.3)                  │  │
│  └─────────────────────┬──────────────────────────────────┘  │
│                        │                                     │
│                        ▼                                     │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ harness (claude / codex / opencode) 対話モード         │  │
│  │   - SKILL.md `boid-kit-init` を Read                   │  │
│  │   - 環境スキャン (which / ls / cat)                    │  │
│  │   - kit.yaml.tmpl を embed 参照                        │  │
│  │   - kit.yaml 群を ~/.local/share/boid/kits/ に書く     │  │
│  └─────────────────────┬──────────────────────────────────┘  │
│                        │                                     │
│                        ▼                                     │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ 後段 scan (本 plan で組み込み口を実装)                  │  │
│  │   - 書かれた kit.yaml 群を orchestrator.ScanSecretsFile│  │
│  │   - 1 件でも finding あれば全体 rollback + error 出力  │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

`boid workspace configure <slug>` も同じ枠で動く。 違いは:

- writable: `~/.config/boid/workspaces/<slug>.yaml` の親 dir RW bind (yaml は host 側で pre-create してから RW bind)
- 追加 bind: 紐付け済み project 群を ReadOnly bind (`package.json` / `go.mod` / hook script の read 用)
- **ServerSocket: `client.DefaultSocketPath()` を設定** (skill が `boid workspace show` / `boid kit list` 等 daemon API を叩くため、 §2.3 参照)
- skill: `boid-workspace-configure`
- 後段 scan の対象: 書かれた `workspace.yaml` 単体

### 2. dispatch 配線

#### 2.1 `cmd/exec.go` の経路を踏襲

`runExec` (`cmd/exec.go:38-130`) で `SessionJobInput` から `BuildExecJobSpec` を呼び `RunOuter` する流れがすでに用意されている。 これを参考に `BuildInitJobSpec(InitJobInput)` を新設する (`internal/dispatcher/init_jobspec.go` 新規)。

```go
type InitJobInput struct {
    Profile        sandbox.Profile  // ProfileInit 固定
    WritableDirs   []string         // 親 dir RW bind 対象
    PreCreateFiles []string         // 起動前に host 側で touch して RW bind するファイル
    ReadOnlyBinds  []string         // configure の場合 project 群
    Argv           []string         // harness binary + prompt
    DisplayName    string
    Env            map[string]string // skill に渡す追加 env (BOID_WORKSPACE_SLUG 等)
}

func BuildInitJobSpec(in InitJobInput) JobSpec { ... }
```

`runKitInit` / `runWorkspaceConfigure` (cmd 側) が:

1. `default_harness` を解決 (§4)
2. **embed 済み skill を host 側に展開** (§3.2、 daemon 起動状態に依存しない)
3. `InitJobInput` を組み立て
4. `BuildInitJobSpec` で JobSpec 作成
5. `SandboxRuntimeInfo` を組み立て、 **`workspace configure` のみ `ServerSocket: client.DefaultSocketPath()` を設定** (§2.3)
6. `RunOuter` で sandbox + harness を foreground 起動 (`cmd/exec.go:139-156` と同じ)
7. exit 後、 書かれた yaml 群を scan (§6)

#### 2.2 触らないところ

- `Broker` 登録は `runner.go:180-183` で ProfileInit ガード済 → CLI 側で broker 関連スキップだけ追加 (`brokerSocket = ""`, `brokerToken = ""`)
- 既存 `SessionJobInput` / `BuildExecJobSpec` には**触らない** (exec 経路の余計な regression を避ける)。 似た構造を持つ別関数として並べる方が安全

#### 2.3 ServerSocket 配線 (Codex 第 1 ラウンド指摘 3 対応)

| コマンド | ServerSocket | 理由 |
|---|---|---|
| `boid kit init` | **不要** (空文字、 bind しない) | host scan + kit.yaml write のみ。 daemon API を叩かない設計 (daemon 未起動な初手オンボーディングでも動かす要求) |
| `boid workspace configure <slug>` | **必須** (`client.DefaultSocketPath()`) | skill が `boid project list` / `boid kit list` 等 daemon API を叩く |

`sandbox_builder.go:257-264` の既存ロジック (`rt.ServerSocket != ""` のときだけ `/run/boid/server.sock` を bind + `BOID_SOCKET` env 注入) をそのまま使う。 `kit init` 側で空文字にすれば bind されず、 daemon 未起動でも sandbox 起動が走る。 環境固有な socket path (custom `BOID_SOCKET` 等) もこの経由で正しく反映される。

### 3. skill embed 戦略

#### 3.1 embed 構造

```
internal/skills/data/
├── boid-orchestrate/        (既存)
├── boid-task/               (既存)
├── boid-web/                (既存)
├── boid-kit-init/           (新規)
│   ├── SKILL.md             (スキャン手順 + 雛形参照プロトコル)
│   └── templates/
│       ├── node.yaml.tmpl
│       ├── go-dev.yaml.tmpl
│       ├── github-cli.yaml.tmpl
│       ├── docker.yaml.tmpl
│       ├── volta.yaml.tmpl
│       └── ...
└── boid-workspace-configure/ (新規)
    └── SKILL.md             (project スキャン + kit カタログマッチング手順)
```

`workspace-configure` 側に templates は不要 (kit カタログを読むだけで雛形は持たない)。

#### 3.2 既存 `skills.DeployAll` を流用 + CLI 側でも呼ぶ (Codex 第 1 ラウンド指摘 1 対応)

`internal/skills/deploy.go:13` の `//go:embed` ディレクティブに 2 ディレクトリ追加すれば、 daemon 起動時に `~/.local/share/boid/skills/` 配下へ展開される (`internal/server/server.go:68`)。 adapter は `~/.local/share/boid/skills/<name>` を `~/.claude/skills/<name>` に bind する (`internal/adapters/claude/bindings.go:53` 他)。

**ただし**: `boid kit init` はオンボーディング初手になり得るため、 **daemon 未起動 → skill 未展開 → adapter bind 先が空ディレクトリ → harness が SKILL.md を見つけられない**、 という詰みパスがある。

対策: CLI 側 (`runKitInit` / `runWorkspaceConfigure`) で sandbox 起動直前に **`skills.DeployAll(defaultSkillsDir())` を idempotent に呼ぶ**。 `DeployAll` は冪等で内容変更検出付き (`deploy.go` 既存実装)、 daemon が後から起動しても再展開で上書き競合は起きない (host 側 `~/.local/share/boid/skills/` 1 経路に集約されている)。

副次的に、 既存 `boid exec` 経路で daemon が一度も起動してない状態だと同じ詰みパスがあったはずだが、 そちらは通常 daemon 起動済を前提とするので顕在化していなかった。 本 plan で `kit init` を入り口に据えるなら CLI 展開を入れる。

#### 3.3 SKILL.md 内容 (`boid-kit-init`)

骨子 (`kit-workspace-project-reorg.md:578-592` を肉付け):

- **役割宣言**: 「いまこのマシンで利用できる kit を収拾する。 project は見ない (project マッチングは workspace configure の責務)」
- **スキャン手順**: PATH binary 一覧 + `$HOME` 配下の標準ディレクトリチェック
- **検出ヒューリスティック**: 詳細表 (volta / nvm / system node, gh, docker socket, go, …)
- **雛形参照プロトコル**: `~/.claude/skills/boid-kit-init/templates/<name>.yaml.tmpl` を Read → 検出した実値で `{{volta_home}}` 等を置換 → `~/.local/share/boid/kits/<name>/kit.yaml` に書く
- **衝突対話**: 既存 kit dir があれば `meta.generated_at` を読んで「YYYY-MM-DD に生成された node kit があります、 上書きしますか?」 とユーザに聞く
- **secret-free 規約**: `host_commands[*].env` の値は `secret:<key>` 参照のみ、 生 secret 値は絶対書かない (後段 scan で refuse される旨を明記)

#### 3.4 SKILL.md 内容 (`boid-workspace-configure`)

骨子:

- **役割宣言**: 「workspace に紐付け済み project 群をスキャンし、 必要 kit を workspace.yaml `kits:` に追加」
- **入力**: `BOID_WORKSPACE_SLUG` 環境変数 (CLI が JobSpec.Env に注入)
- **手順** (Codex 第 1 ラウンド指摘 2 対応で訂正、 `boid project list --workspace <slug>` は現行存在しないため):
  1. **`boid workspace show <slug> --json`** で `WorkspaceShowView.projects[]` を取得 → `WorkDir` 抽出 (`cmd/workspace.go:181-265` 既存経路、 daemon API + workspace.yaml を統合した view が返る)
  2. 各 project の `package.json` / `go.mod` / `task_behaviors[].hooks` script を read
  3. **`boid kit list`** でカタログ列挙 (`cmd/kit.go:36-54`)、 各 kit dir の `kit.yaml` を直接 read してマッチング (CLI 経由の `boid kit show` は未実装、 本 plan で必要なら追加検討)
  4. workspace.yaml 既存内容を read (あれば diff 提示)
  5. workspace.yaml に `kits:` array 追加 (env / capabilities は user 既存値温存)
- **足りない kit のガイダンス**: 「project が gh 要求してるけど github-cli kit がカタログに無い、 `boid kit init` 再実行してね」 を出力
- **secret-free 規約**: workspace.yaml `env:` には plain k/v のみ、 secret は kit 側で完結

**`boid project list --workspace` を新設する案 (採否は実装時判断)**: skill 側手順をより素直にするため、 `cmd/project.go:62` の `projectListCmd` に `--workspace <slug>` flag を足して `GET /api/projects?workspace_id=<slug>` を叩く方が SKILL.md は短く書ける (server 側 query は `internal/api/project.go:85` で対応済)。 ただし `workspace show --json` の view を流用すれば本 plan の射程内で完結する。 PR4 設計時に skill 側コードを書きながら判断する。

#### 3.5 雛形 (`kit.yaml.tmpl`) 構文

Go の `text/template` を借りるが、 **エージェントは Go コードを書かない**。 雛形は最終的に sandbox 内で agent が cat → 変数置換 → write する文字列であり、 `{{volta_home}}` 等は agent が手で書き換える。 ファイル形式としては:

```yaml
# Template for: node (volta variant)
# Detection signal: which volta succeeded
# Variables to substitute:
#   {{volta_home}} = $VOLTA_HOME or $HOME/.volta
#   {{node_version}} = detected current version
meta:
  name: node
  description: Node.js toolchain (volta-managed)
host_commands:
  node:
    path: {{volta_home}}/bin/node
    args: []
  npm:
    path: {{volta_home}}/bin/npm
    args: []
env:
  VOLTA_HOME: {{volta_home}}
additional_bindings:
  - {{volta_home}}
```

エージェントが Read してテキスト置換するだけのフォーマット (Go の text/template engine は使わない、 つまり Go コード非依存)。

### 4. default_harness 設定 (Q4 への答え)

**設計**:

- `~/.config/boid/config.yaml` (新規) または既存設定ファイルに `default_harness: claude` キーを追加
- 環境変数 `BOID_DEFAULT_HARNESS=claude` で override 可
- 解決順: env > config file > built-in default (`claude`)
- 未インストール harness が選ばれた場合は CLI で early error + インストール案内

**実装**:

- `internal/config` パッケージに `DefaultHarness() string` 追加
- `cmd/kit.go` / `cmd/workspace.go` が呼び出す
- 既存 `config.yaml` 系のパース体系を流用 (`internal/config/load.go` 周辺、 実装時に確認)

**範囲外**: 「対話モードでどう起動するか」 は `internal/adapters/<harness>/run.go` の既存 `Run` を流用 (multi-harness task hook plan で揃った経路を再利用)。 ハーネス別の prompt 形式は adapter 側が吸収する。

### 5. write 経路 (Q5 への答え)

`kit init`:

- 親 dir `~/.local/share/boid/kits/` を **RW bind**
- sandbox 内 agent は `~/.local/share/boid/kits/<name>/kit.yaml` を自由に新規作成
- 親 dir bind なので未存在の子ディレクトリ作成 OK

`workspace configure <slug>`:

- ホスト側 CLI が**先に空ファイル touch** (`~/.config/boid/workspaces/<slug>.yaml` を `0o600` で create)
- そのファイル単体を **RW bind**
- sandbox 内 agent は touch 済ファイルに対して overwrite (rename-into-place は使えない、 truncate + write のみ)

**rationale**: workspace.yaml は 1 ファイル限定なので親 dir RW bind より file RW bind の方が攻撃面が狭い。 kit init は複数 kit dir を作るので親 dir RW bind 必須。

**create-then-bind の事前 fail-safe**:

- ホスト側 touch 時に既存ファイルがあれば内容を temp に backup (`<slug>.yaml.bak.<unixtime>`)
- agent 失敗時に `bak` を読んで rollback
- agent 成功時に backup 削除

### 6. 後段 scan の組み込み口 (PR1 #642 との合流)

- `BuildInitJobSpec` 経路の最後で、 host 側から書かれた yaml を `orchestrator.ScanSecretsFile` で scan
- finding 1 件でも出たら:
  - 書かれた yaml を**全削除** (kit init は新規 kit dir のみ削除、 既存 kit は触らない)
  - `workspace configure` は `bak` から rollback
  - stderr に redact 済 finding 列挙
  - exit 1
- finding 無し:
  - kit init: stdout に「生成された kit: node, go-dev, …」 を表示
  - workspace configure: stdout に「kits: [node, go-dev], …」 を表示 + `boid project reload` を内部実行

### 7. CLI UX

```sh
# kit init (引数なし固定)
$ boid kit init
[scanning host environment...]
[detected: volta (~/.volta), gh, docker, go 1.24]
[entering interactive harness session for kit generation]
... (harness 対話) ...
[generated: node, github-cli, docker, go-dev]
[secret scan: clean]

# workspace configure (slug 必須)
$ boid workspace configure dev
[scanning workspace 'dev' projects: boid, boid-kits]
[entering interactive harness session for workspace configuration]
... (harness 対話) ...
[written: ~/.config/boid/workspaces/dev.yaml]
[secret scan: clean]
[reloaded projects]
```

## PR シリーズ案

| # | 内容 | 規模 | 依存 |
|---|---|---|---|
| PR1 | secret scan pkg | 小 | — | **landed (#642)** |
| PR2 | default_harness 設定 + 解決ヘルパ (`internal/config`) | 小 (1d) | — |
| PR3 | `BuildInitJobSpec` + `cmd/kit.go` 配線 (harness セッション起動できるところまで、 skill は dummy) | 中 (2d) | PR2 |
| PR4 | `boid-kit-init` SKILL.md + templates/ (雛形 5-6 個、 stub kit カタログ) | 中 (2-3d) | PR3 |
| PR5 | 後段 scan 組み込み + rollback ロジック | 小 (1d) | PR1, PR4 |
| PR6 | `BuildInitJobSpec` 流用で `workspace configure` 配線 | 小 (1d) | PR3, PR5 |
| PR7 | `boid-workspace-configure` SKILL.md | 中 (2d) | PR6 |
| PR8 | e2e scenario (fake `which volta` 等で固定環境作って kit init を回す) | 中 (2d) | PR4, PR7 |
| PR9 | docs (`docs/ja/guide/onboarding.md` 更新) | 小 (0.5d) | PR7 |

合計 **9 PR / 約 2 週間**。 PR2-PR5 は kit init 縦串、 PR6-PR7 は workspace configure 縦串、 PR8-PR9 は仕上げ。

## スコープ外

- ハーネス未インストール時の自動インストール (CLI は案内のみ)
- 既存 kit の自動 garbage collection (`kit remove` で十分とする)
- 雛形の包括的カバレッジ — 初稿は node / go / gh / docker / volta の 5 個に限定、 他は手書きを案内
- shell adapter 経路の `Visibility.KitRoots` 配線 (Q2、 既に動いてる)
- 「project と workspace の kit ミスマッチ事前検出」 (= 旧 plan の capability provides/requires 抽象、 採用しない方針)

## オープンな宿題

- `default_harness` 未設定時の挙動 (init wizard 中で聞く? built-in `claude` 固定?)
- 雛形の `meta.signals` フィールド設計 (`workspace configure` がカタログマッチングで読む構造、 PR4 設計時に詰める)
- e2e で対話 harness を fake する手段 (claude-stub を作るか、 ProfileInit のサンドボックスのみ検証して agent 部分は skip するか)

## 改訂履歴

- **第 1 ラウンド** (2026-06-26) — 設計プラン初稿
- **第 2 ラウンド** (2026-06-26) — Codex 第 1 ラウンドレビュー反映:
  - 指摘 1: `DeployAll` が daemon 起動依存だと初手オンボーディングで詰む点 → §3.2 で **CLI 側でも `skills.DeployAll` を冪等に呼ぶ**設計を明示、 §1 アーキテクチャ図にもステップ追加
  - 指摘 2: `boid project list --workspace <slug>` が現行 CLI に存在しない点 → §3.4 を **`boid workspace show <slug> --json` 経由**に訂正、 補助オプションで `project list --workspace` 新設検討の余地を残す
  - 指摘 3: workspace configure が daemon API を叩く前提なのに `ServerSocket` mount 配線が無い点 → §2.1 で `InitJobInput.Env` 追加、 §2.3 **「ServerSocket 配線」 節を新設** (`kit init` は空 / `configure` は `client.DefaultSocketPath()`)、 §1 アーキテクチャ図にも反映
