# `project.yaml` リファレンス

プロジェクトのルートに置く `.boid/project.yaml` の全フィールドをまとめたリファレンスです。

このページは仕様の網羅を目的にしています。 用語の解説は [概念](../guide/concepts.md) を、 動かし方は [Getting started](../getting-started/) を参照してください。

## 役割と配置

- パス: プロジェクトルート直下の `.boid/project.yaml`
- 役割: そのディレクトリを `boid` プロジェクトとして登録し、タスクの種類 (behavior) と、 プロジェクトで使う拡張パッケージ (kit) を宣言する
- 登録: `boid project add <project-root>` で `boid` の DB に取り込まれる
- 変更後の反映: `boid project reload` で再読み込みする

## 最小例

```yaml
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
```

## トップレベルのフィールド

| キー | 型 | 必須 | 役割 |
|---|---|---|---|
| `id` | string | はい | `boid` 内でプロジェクトを一意に識別する文字列。タスク作成時に `project_id` で参照される |
| `name` | string | はい | UI で表示するプロジェクト名 |
| `worktree` | bool | `false` | 以前は `true` で executor / supervisor タスクに専用の isolated branch (`boid/<id8>`) を割り当てていた。 **docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) で per-task branch と fork point 概念が廃止**され、 root / child を問わず全タスクが sandbox 内 clone 上で `base_branch` を直接 checkout するようになったため、 このフィールドは現在 checkout 挙動に影響しない (スキーマ上は引き続き受理される)。 詳細は [タスク種別と HEAD branch](#タスク種別と-head-branch) を参照 |
| `base_branch` | string | (省略時は後述) | PR ターゲットとなるベースブランチ。 タスク作成時に解決して row に保存される。 **省略時**: root task は daemon の現 HEAD branch (`${current_branch}` 相当) に展開; child task は親の `base_branch` を継承。 detached HEAD で root task 作成時に省略すると 400 エラー。 `${TASK_REMOTE_ID}` / `${current_branch}` の展開をサポート (後述 [動的 base_branch](#動的-base_branch)) |
| `fork_point` | string | (省略時 `origin/HEAD` フォールバック) | `base_branch` がまだローカル / origin のどちらにも存在しない状態 (case 3) で branch を作るときの fork 起点。 任意の `git rev-parse --verify` で解決可能な ref を指定 (branch / tag / SHA / `origin/main` など)。 **未設定時は `refs/remotes/origin/HEAD` にフォールバック**。 origin/HEAD も未設定なら case 3 はエラー (`git remote set-head origin --auto` を実行するか、 `fork_point` を設定する)。 **project root の作業ツリー HEAD は意図的に参照されない** — タスク作成からディスパッチまでの間にユーザが root で別 branch をチェックアウトしていても、 fork 起点が暴れない。 詳細は [`fork_point` と case 3](#fork_point-と-case-3) を参照 |
| `kits` | KitRef のリスト | いいえ | プロジェクト全体で読み込む kit。 全 behavior で共通に使われる |
| `task_behaviors` | map (string → TaskBehavior) | はい | このプロジェクトで作れる「タスクの種類」一覧 |
| `host_commands` | HostCommands | いいえ | サンドボックスから host へ流せる外部コマンドの宣言 |
| `additional_bindings` | BindMount のリスト | いいえ | サンドボックスにマウントしたい追加パス |
| `env` | map (string → string) | いいえ | サンドボックス内に流す環境変数 |
| `secret_namespace` | string | いいえ | このプロジェクトの secret を解決する際のネームスペース |
| `capabilities` | Capabilities | いいえ | サンドボックスのオプション機能を宣言する。現在サポートする機能は `docker` のみ |
| `default_task_behavior` | string | いいえ | `boid task create` で `--behavior` を省略したときに使う behavior の名前。未指定の場合は `task_behaviors` に `supervisor` があれば暗黙で使う (WARN あり)、なければエラー |

## git gateway / sandbox 内 clone

project が可視なジョブ (hook / セッション / `boid exec` を問わず) は、 毎回 **daemon 内の git gateway** (認証注入リバースプロキシ) 経由で project を sandbox 内に新規 clone します。 host 側の project ディレクトリを直接マウントしたり、 host 側に git worktree を作ったりはしません。

- clone の origin は gateway を指す。 sandbox 内の git は credential レスの素の git バイナリで、 fetch / push はすべて gateway が上流 (GitHub 等) への認証注入を代行する
- **成果の共有は origin への push が唯一の経路**: commit しただけの変更は他セッション・他ホストには一切共有されない。 「done 前に push」が前提になる
- `readonly: true` の behavior では clone 自体はローカルに書き込めるが、 push が gateway 側で拒否される (fetch はできる)。 「何も書けない」ではなく「境界を越えられない」という読み書き対称な適用に変わった
- reopen は「再 clone + branch checkout」として実行される。 保証されるのは commit (+ push) 済みの内容のみ
- 同一 project・同一 HEAD branch を対象とする複数タスクも、 それぞれ独立した clone を持つため **並行して dispatch される** (以前あった branch 単位の直列ロックは廃止済み)。 同時に push すると通常の git のとおり non-fast-forward で reject されるので、 fetch + merge/rebase して再 push する
- workspace peer project は fetch-only でサンドボックス内から clone・reference 可能。 書き込みが必要な場合は peer への cross-project child task を作る

## `task_behaviors.<name>`

map のキーが behavior の識別子で、 タスク作成時に `behavior:` で指定する名前です。 **canonical な名前は 2 つ** です:

| 名前 | 役割 |
|---|---|
| `supervisor` | readonly な統括役。 要求を triage し、 child executor task を作り、 監視する。 ファイル編集はしない |
| `executor` | 書き込み可能な実装役。 単一の集中したタスクを受けて成果物 (commit / PR / payload trait) を作る |

canonical 以外の任意のキー名も使用できます (Track A2 以降、`readonly` の既定値は `true` (fail-safe) です。writable にするには `readonly: false` を明示してください)。 レガシーキー `plan` (`supervisor` の alias) と `dev` (`executor` の alias) はマイグレーション期間中は引き続き受け付けますが、 deprecated です。

各 behavior エントリの設定項目:

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `readonly` | bool | `true` (fail-safe) | タスクのワークディレクトリを read-only マウントするかどうか。`executor` のみ互換 override で `false` が保たれる (WARN あり)。 writable な behavior には `readonly: false` を明示する |
| `traits` | string のリスト | (空) | この behavior のタスクが扱う payload trait の宣言 (例: `[artifact]`) |
| `default_instruction` | Instruction | (空) | タスク作成時の active instruction として `Task.Instructions` 配列に積まれる雛形 (単一 Instruction object) |
| `kits` | KitRef のリスト | (空) | この behavior だけに追加で読み込む kit。 プロジェクトトップの `kits` リストとマージされる |

> **注意:** `task_behaviors.<name>` 配下に `name` フィールドを書いてもローダーに無視されます。 behavior の識別子はマップキーを使ってください。

### 動的 `base_branch`

`base_branch` には 2 つの interpolation token が使えます:

- `${TASK_REMOTE_ID}` — 親 supervisor がこのタスクに記録した remote 識別子 (GitHub PR 番号など)。 supervisor / executor 双方で解決される。 "1 Supervisor 1 PR" ワークフロー ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) で、 supervisor セッションごとに専用の統合ブランチを切るために使う
- `${current_branch}` — タスク作成時に project リポジトリの daemon の HEAD ブランチに解決される

**省略時の解決優先順位:**

1. `parent_id` あり (child task): 親タスクの `base_branch` をそのまま継承。 template 展開は行わない
2. `parent_id` なし + `base_branch` 省略 (root task): 作成時点の `${current_branch}` に展開してから row に保存。 detached HEAD の場合は 400 エラー
3. `parent_id` なし + `base_branch` 指定: template 展開 (`${TASK_REMOTE_ID}` / `${current_branch}`) を行う

エンドツーエンドの例 ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) は [docs/workflows.md](../../workflows.md) を参照。

`worktree` フィールドの経緯については [概念 / worktree](../guide/concepts.md#worktree) を参照してください。

### `fork_point` と case 3

`base_branch` (テンプレート展開後) がローカルにも `origin/<base>` にも存在しない状態を **case 3** と呼びます ([base_branch_classify.go](../../../internal/orchestrator/base_branch_classify.go))。 この場合 runner は sandbox 内 clone の中でそのブランチを **新規ローカル branch として作成** します (host 側の project ディレクトリは一切参照しません)。 問題は「どこから fork するか」で、 host git worktree を使っていた旧実装では project root の HEAD を起点にしていたため、 タスク作成からディスパッチまでの間にユーザが root で別 branch をチェックアウトしていると、 想定外の commit から base が切られる事故がありました。

新しい解決順:

1. **`fork_point` が project.yaml に設定されていれば** その ref を起点にする。 `git rev-parse --verify` で解決できるものなら何でも可 (branch / tag / commit SHA / `origin/main` 等)。 解決失敗時は明確なエラー
2. 未設定なら **`refs/remotes/origin/HEAD`** にフォールバック。 通常は `git clone` が自動で設定する。 既存 repo で未設定なら `git remote set-head origin --auto` で一度設定する
3. どちらも解決できなければ case 3 はエラー。 project root の HEAD は意図的に参照されない

典型的な使い分け:

- 普通の GitHub プロジェクトで `main` が default branch: 設定不要 (origin/HEAD で自動的に解決される)
- default branch が `master` や `develop` 等: 設定不要 (origin/HEAD が指してれば OK)
- リモートが無い / origin/HEAD を設定できない: `fork_point: main` のように明示
- 特殊な default 起点を使いたい: `fork_point: origin/release/2026` のように明示

### タスク種別と HEAD branch

**docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) で per-task branch (`boid/<id8>`) と fork point 概念は廃止されました。** タスク種別 (root / child、 supervisor / executor) に関わらず、 sandbox 内 clone は常に `task.BaseBranch` を直接 checkout します。 worktree 時代に必要だった「child は隔離用の専用 branch を切る」という仕組みは、 clone 自体が isolation 単位になったことで不要になりました — 同じ branch 名を別々の sandbox 内 clone で checkout しても衝突しません。

| タスク種別 | HEAD branch | readonly |
|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | sup=true / exec=false |
| **child sup / child exec** | `task.BaseBranch` | sup=true / exec=false |

- **root タスク** (`parent_id == ""`): sandbox 内 clone した上で `base_branch` を直接 checkout する (新規 branch は作らない)。 `base_branch` が origin にまだ存在しない場合 (case 3) は [`fork_point` と case 3](#fork_point-と-case-3) の解決結果からローカル作成する
- **child タスク** (親あり): root タスクと全く同じ扱いで `base_branch` を直接 checkout する。 `base_branch` を省略すると親タスクの `base_branch` をそのまま継承する (template 展開なし、 [動的 base_branch](#動的-base_branch) 参照) ため、 明示指定がない限り親子は同じ branch 名を checkout する
- `task.BaseBranch` は PR target として全子タスクに継承され、 `BOID_BASE_BRANCH` env で executor に渡る

**並列に走る兄弟 executor が同じ base_branch へ同時に push すると衝突します**。 これは isolation の欠如ではなく、 従来から変わらない executor 側の rebase/retry 契約です (下記「同一 HEAD branch を対象とする複数タスクの並行実行」参照)。 真に isolate したい場合は、 supervisor が子ごとに異なる `base_branch` を明示指定してください (例: `feature/BGO-214`, `feature/BGO-215`, `feature/BGO-216`)。

### 同一 HEAD branch を対象とする複数タスクの並行実行

以前は同一 `<projectID>:<HEAD branch>` を対象とする複数タスクを FIFO ロックで直列化していました (同じ host git worktree を複数タスクが同時に使えないため)。 **この直列ロックは git gateway cutover で廃止済みです**: 各タスクは独立した sandbox 内 clone を持つため、 同じ branch を対象とする複数タスクも並行して dispatch されます。 同時に push した場合は通常の git のとおり non-fast-forward で reject されるので、 fetch + merge/rebase して再 push してください。 意図的に競合を避けたい場合は、 前節のとおり子ごとに異なる `base_branch` を割り当ててください。

### 依存子の最新化とマージ責務

boid コアは子タスクの dispatch 順序や base 同期には関与しません。 sub-sup (子 supervisor) が子タスクの dispatch 順序を制御しますが、 clone モデルでは sub-sup 自身が更新すべき「自分の working branch」はもう存在しません — sub-sup 自身も毎回 `base_branch` を直接 checkout するだけの読み取り専用 clone です:

```
A (executor) が done → A の PR を base_branch へ merge (origin 上)
                         ↓
            sub sup が B を dispatch → B の clone は origin から新規 fetch するため
                                        A の merge 済み内容を自動的に含む
```

merge のタイミング・コマンド・対象は **project 側 instruction の責務** であり、 skill / boid コアには記述しません。 boid コアの関与は `BOID_BASE_BRANCH` env を渡すことに限定されます (`BOID_PARENT_BRANCH` は docs/plans/branch-policy-simplification.md Phase 1 で廃止されました — production project.yaml / e2e script に実利用が確認されなかったため)。

### `default_instruction`

単一の Instruction object です。 タスク作成時に Task.Instructions 配列に append され、 これが executing で agent に渡される最初の active instruction になります。

reopen 時は `boid task reopen <id> --message "..."` で新しい Instruction を append し、 配列の最後の要素 (= 直近の active instruction) が agent に渡されます。 `agent` / `model` は前回 active から継承されます。

## 共通の構成要素

### KitRef

`kits` フィールドの各要素は次のどちらかで書けます。

- 文字列: `github.com/<owner>/<repo>/<sub-path>` の形 (例: `github.com/novshi-tech/boid-kits/claude-code`)
- map 形式:
  ```yaml
  kits:
    - ref: github.com/novshi-tech/boid-kits/claude-code
      as: agent
  ```
  `as` で alias を付けると、別の kit と agent 名が衝突するときに区別できます

`<sub-path>` は省略可。リポジトリ直下に kit がある場合は不要です。

### HostCommands

サンドボックスは既定では host のコマンドを呼べません。 `host_commands` で許可リストを宣言した分だけ通します。 リストとマップの 2 種類の書き方があります。

リスト形式 (制約なしで許可):

```yaml
host_commands:
  - gh
  - aws
```

マップ形式 (各コマンドに細かい制約をかけられる):

```yaml
host_commands:
  gh:
    allow: [pr, issue, run]
    deny: ["* delete*"]
    env:
      GH_REPO: ${boid:repo_slug}
    reject:
      - match: "*--body-file*"
        reason: 'サンドボックスのファイルパスは host からは見えない。--body "$(cat <file>)" で内容を渡す'
  aws:
    path: /usr/local/bin/aws
    env:
      AWS_REGION: ap-northeast-1
```

各エントリ (`HostCommandSpec`) のフィールド:

| キー | 型 | 役割 |
|---|---|---|
| `allow` | string のリスト | 許可するサブコマンドまたはグロブパターン (`* ?` 含むパターンとして自動判別) |
| `deny` | string のリスト | 拒否するパターン (allow より優先) |
| `reject` | RejectRule のリスト | `match` (glob) にマッチした呼び出しを `reason` 付きで拒否する。 `reason` は必須で、 拒否時に `host_commands.<name>: rejected: <reason>` としてエージェントへ返る (下記「host command の実行契約」参照) |
| `stdin` | bool | **非推奨**。 パースはされるが常に無視される (下記「host command の実行契約」参照) |
| `path` | string | バイナリの絶対パス (host の `$PATH` 解決を上書きしたい場合) |
| `env` | map (string → string) | このコマンド呼び出し時に追加する環境変数。 値に `${boid:repo_slug}` と書くとコンテキスト変数として展開される (下記「host command の実行契約」参照) |

特殊な使い方として、 `path` に kit / プロジェクト内の相対パスを書くと、その path のコマンドだけがサンドボックスから host へ流れます (例: `path: e2e/run.sh`)。

> **予約名:** `git`、`boid`、`fetch` はサンドボックス組み込みコマンドです。 `host_commands` に宣言しても無視されます。

#### host command の実行契約

- **stdin は渡らない** — サンドボックス shim は stdin を読まず、 broker も受け取っても捨てる。 `stdin: true` は設定として受理されるが効果はない (deprecation warning が出る)。 ファイル内容や長文をコマンドへ渡したい場合は stdin ではなく引数 (例: `--body "$(cat <file>)"`) を使う
- **cwd は中立ディレクトリ固定** — host command は host 側で project の checkout ディレクトリではなく中立ディレクトリ (`os.TempDir()`) で実行される。 cwd から repo を推定する動作 (`gh` の暗黙 `-R` 等) には依存できない
- **repo 文脈は env で渡す** — cwd 推定の代わりに、 `env:` の値に `${boid:repo_slug}` と書くとトークン登録時に project の origin remote から導出した `host/owner/repo` 形式の文字列に展開される。 `gh` であれば `GH_REPO: ${boid:repo_slug}` で従来どおり透過的に動く
- **reject ルール** — `match` (allow/deny と同じ glob 意味論、 joined args に対して) にマッチした呼び出しは shim (早期) と broker (権威) の両方で拒否され、 `host_commands.<name>: rejected: <reason>` というメッセージがエージェントに返る。 `reason` は代替手段を書くこと (単に「使えません」ではなく次に何をすべきか)

`local/<name>` 形式の kit 参照 (例: `local/my-kit`) は、 プロジェクトルート相対でローカル kit ディレクトリを解決します。 リモートレジストリに公開せずに kit を開発する場合に便利です。

### BindMount

`additional_bindings` の各要素はサンドボックスにマウントしたい host 上のパスを表します。

```yaml
additional_bindings:
  - source: ${HOME}/.local/share/some-tool
  - source: ${HOME}/.config/some-tool
    mode: rw
  - source: ${HOME}/.netrc
    is_file: true
    optional: true
  # gitignored だがサンドボックス内の clone からも参照させたいファイル (例: .NET の global.json)
  - source: ${PROJECT_WORKDIR}/global.json
    target: ${WORKTREE}/global.json
    is_file: true
    optional: true
```

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `source` | string | (必須) | host 側のパス。 `${HOME}` 等の展開可 |
| `target` | string | `source` と同じ | サンドボックス内のマウント先パス |
| `mode` | string | `""` (ro) | `rw` で読み書き可。空文字列なら読み取り専用 |
| `is_file` | bool | `false` | source がファイルの場合 `true` |
| `optional` | bool | `false` | host に source が無くてもエラーにせずスキップする |

#### 動的トークン: `${WORKTREE}` / `${PROJECT_WORKDIR}`

`source` / `target` では通常の環境変数 (`${HOME}` 等) に加え、 boid が dispatch 時に解決する 2 つの動的トークンが使えます:

- `${PROJECT_WORKDIR}` — host 側のプロジェクトディレクトリ (例: `/home/you/src/your-project`)
- `${WORKTREE}` — タスクが実行されるサンドボックスの cwd。 project が可視なジョブ (git gateway 経由で sandbox 内に project が clone される場合) では sandbox 内 clone 先のパス (例: `/workspace/<project-name>`) に、 project が可視でないジョブでは `${PROJECT_WORKDIR}` と同じ値に解決される

主な用途は、 `.gitignore` してあるが sandbox 内の clone からも参照させたいファイル (`.NET` の `global.json`、 `.env.local`、 `appsettings.Development.json` など) をホストの project workdir からサンドボックス内の clone に bind することです。

`target` を **明示** し、 展開後 `source` と等値になった binding は self-mount を避けるため自動的に skip されます。 上の例の binding は:

- clone-mode のジョブ (project が可視な hook / session / exec のほぼ全て) では `/host/proj/global.json` → `/workspace/proj/global.json` のように別パスへ bind され、
- clone-mode でないジョブ (project 不可視、または dispatcher のテスト配線等) では同一 path に潰れて skip される (project ディレクトリは既に projectVisibilityMounts でサンドボックスに見えているため不要)

ので、 同じ宣言で clone モードかどうかに依存せず動作します。

> **注意:** `workspace.yaml` での `additional_bindings` エントリには `mode` を明示 (`ro` または `rw`) する必要があります。 空文字列は受け付けられません。

### Instruction

`default_instruction` に書く構造体です。

```yaml
default_instruction:
  agent: claude-code
  model: sonnet
  message: |
    ...
```

| キー | 型 | 役割 |
|---|---|---|
| `agent` | string | この instruction を受け取る harness の識別子。`claude-code` は claude harness (boid 本体 builtin)、`codex` は builtin codex adapter、`opencode` は builtin opencode adapter、省略または未知値は shell adapter に fallback する |
| `name` | string | 同じ agent に複数 instruction を渡す場合の識別子 (省略可) |
| `message` | string | agent に渡される指示文 |
| `model` | string | agent が選ぶモデル名 (例: `opus`、 `sonnet`)。 kit 側で解釈される |

> **注意:** `type:` と `interactive:` は `Instruction` のフィールドではなく、 YAML に書いても黙殺されます。

### CommandSpec (廃止)

Phase 3-d (2026-06 リリース) で `commands:` map は廃止されました。 project.yaml / task_behaviors.<name> 配下のいずれに書かれていても **silent に無視され、 起動時に deprecation warning が 1 回出力されます** (boid daemon ログ)。 既存 yaml はそのままでも壊れません。

代替手段:

| 旧 | 新 |
|---|---|
| `boid exec <project_id> <command-name>` で名前付き登録コマンドを起動 | `boid exec -p <project_id> -- <argv...>` で任意 argv を直渡し |
| Web UI の **Commands** ボタンで claude セッションを起動 | Web UI の `/sessions/new` から harness (claude / codex / opencode / shell) を選んでセッション起動。 同等の `POST /api/projects/{id}/sessions` も提供 |
| task 詳細の **Commands** ボタンで behavior commands を実行 | task が要求する継続的な実行は behavior の hooks で記述する。 ad hoc な実行は task に紐付けず `boid exec` でよい |

## capabilities

サンドボックスのオプション機能を有効化するトップレベルのフィールドです。

### `capabilities.docker`

`capabilities.docker: {}` を宣言すると、そのプロジェクトのサンドボックスに **ネイティブ Docker プロキシ** が有効になります。

```yaml
capabilities:
  docker: {}   # 空オブジェクトが有効化マーカー
```

有効化すると boid daemon は自動的に次の処理を行います:

1. サンドボックス専用の proxy socket を起動（`/run/boid/docker-proxy.sock`）
2. その socket をサンドボックスに bind-mount
3. 以下の環境変数をサンドボックスに自動設定

| 環境変数 | 値 |
|---|---|
| `DOCKER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `CONTAINER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` | `/run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_RYUK_DISABLED` | `true` |

docker CLI・Docker SDK・TestContainers はいずれも `DOCKER_HOST` を参照するため、追加設定なしに proxy 経由で動作します。`TESTCONTAINERS_RYUK_DISABLED=true` は TestContainers の Ryuk reaper を無効化します（Ryuk はサンドボックス分離が禁止する docker.sock bind-mount を要求するため。boid は代わりにジョブ終了時にコンテナを掃除します）。

proxy のセキュリティモデル、ボディ検査ルール、コンテナ GC の詳細は [サンドボックス内部実装 / Docker プロキシ](../architecture/sandbox-internals.md#docker-プロキシ-capabilitiesdocker) を参照してください。

#### docker CLI と host_commands の注意

サンドボックス内の docker コマンドは proxy socket (`DOCKER_HOST`) 経由で動作します。**`capabilities.docker` が有効なプロジェクトで `host_commands` に `docker` をサブコマンド制限なしで登録するとエラーになります。** `host_commands` への `docker` 登録はホスト直実行（proxy バイパス）になるため、boid が起動時に拒否します。

エラーメッセージ:
```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

image build だけをホスト側 docker で実行させたい場合は、サブコマンドを制限すれば可能です:

```yaml
host_commands:
  docker:
    allow: [build]   # build サブコマンドのみ許可 (ホスト直実行)
```

ただしこれはホスト直実行なので `--network host` / `--secret` 等のリスクは残ります。通常の `docker run` / TestContainers は proxy 経由で十分動作するため、`host_commands` への `docker` 登録は不要です。

#### rootless Docker の推奨

proxy 自体が第一防衛線ですが、万一 proxy が迂回された場合の影響を限定するため、ホスト側 Docker daemon は **rootless** で動かすことを推奨します。rootless Docker ではコンテナが user namespace 内で動くため、host root へのエスカレーションが原理的に起きません。

```sh
# rootless Docker のセットアップ (初回のみ)
curl -fsSL https://get.docker.com/rootless | sh
# または distro パッケージ: apt install docker-ce-rootless-extras
```

boid は起動時に docker upstream socket を `DOCKER_HOST` 環境変数 → rootless path (`$XDG_RUNTIME_DIR/docker.sock`) → rootful `/var/run/docker.sock` の順で自動解決します。

docker kit (cetusguard ベース) からの移行手順は [Docker プロキシ移行ガイド](../guide/docker-proxy-migration.md) を参照してください。

## プロジェクトローカル設定 (`.boid/project.local.yaml`) — 廃止

> **廃止**: `project.local.yaml` は廃止されました。内容は `workspace.yaml` に集約されます。
> `boid project migrate <dir>` で自動変換できます。詳細は [移行ガイド](../guide/migration.md) を参照してください。

旧スキーマで `project.local.yaml` が担っていた `host_commands` / `additional_bindings` / `env` / `secret_namespace` は、
現在は `workspace.yaml` で設定します。`workspace.yaml` は machine-local であり `gitignore` 対象です。

## 例: 実プロジェクトの構成

`boid` 自身のリポジトリにある `.boid/project.yaml` (抜粋) を載せておきます。 2 つの behavior (`supervisor`, `executor`) を定義しています。

```yaml
id: boid
name: boid

# worktree はレガシーフィールド (branch-policy-simplification Phase 1 以降、
# checkout 挙動には影響しない)。 root / child を問わず全タスクが sandbox 内
# clone 上で base_branch を直接 checkout する — host 側に git worktree は
# 作らない。
worktree: true

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/go-dev
  - github.com/novshi-tech/boid-kits/github-cli

host_commands:
  playwright-cli:
    allow: ['*']
  run-e2e:
    path: e2e/run.sh

task_behaviors:
  executor:
    name: executor
    default_instruction: { ... }
  supervisor:
    name: Supervisor
    default_instruction: { ... }
```

このスキーマで作れる 3 種類のワークフローの例は [ワークフロー](../../workflows.md) を参照してください。
