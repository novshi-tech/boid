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
| `worktree` | bool | `false` | `true` にすると、 executor / supervisor タスクに専用の git worktree を割り当てる。 **root タスク** (親なし) の HEAD は `base_branch` そのもの (case 1 = project HEAD と一致する場合のみ project root で動き worktree なし)。 **child タスク** (親あり) は常に `boid/<task_id8>` branch の worktree を持つ。 詳細は [タスク種別と worktree HEAD](#タスク種別と-worktree-head) を参照 |
| `base_branch` | string | (省略時は後述) | PR ターゲットとなる worktree のベースブランチ。 タスク作成時に解決して row に保存される。 **省略時**: root task は daemon の現 HEAD branch (`${current_branch}` 相当) に展開; child task は親の `base_branch` を継承。 detached HEAD で root task 作成時に省略すると 400 エラー。 `${TASK_REMOTE_ID}` / `${current_branch}` の展開をサポート (後述 [動的 base_branch](#動的-base_branch)) |
| `kits` | KitRef のリスト | いいえ | プロジェクト全体で読み込む kit。 全 behavior で共通に使われる |
| `task_behaviors` | map (string → TaskBehavior) | はい | このプロジェクトで作れる「タスクの種類」一覧 |
| `commands` | map (string → CommandSpec) | いいえ | サンドボックス内から `boid exec` 経由で呼べる名前付きコマンド |
| `host_commands` | HostCommands | いいえ | サンドボックスから host へ流せる外部コマンドの宣言 |
| `additional_bindings` | BindMount のリスト | いいえ | サンドボックスにマウントしたい追加パス |
| `env` | map (string → string) | いいえ | サンドボックス内に流す環境変数 |
| `secret_namespace` | string | いいえ | このプロジェクトの secret を解決する際のネームスペース |

## `task_behaviors.<name>`

map のキーが behavior の識別子で、 タスク作成時に `behavior:` で指定する名前です。 **サポートする名前は 2 つだけ** です:

| 名前 | 役割 |
|---|---|
| `supervisor` | readonly な統括役。 要求を triage し、 child executor task を作り、 監視する。 ファイル編集はしない |
| `executor` | 書き込み可能な実装役。 単一の集中したタスクを受けて成果物 (commit / PR / payload trait) を作る |

各 behavior エントリの設定項目はわずかです:

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `name` | string | キー名 | UI 表示用のラベル (省略可) |
| `traits` | string のリスト | (空) | この behavior のタスクが扱う payload trait の宣言 (例: `[artifact]`) |
| `default_instruction` | Instruction | (空) | タスク作成時の active instruction として `Task.Instructions` 配列に積まれる雛形 (単一 Instruction object) |

### 動的 `base_branch`

`base_branch` には 2 つの interpolation token が使えます:

- `${TASK_REMOTE_ID}` — 親 supervisor がこのタスクに記録した remote 識別子 (GitHub PR 番号など)。 supervisor / executor 双方で解決される。 "1 Supervisor 1 PR" ワークフロー ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) で、 supervisor セッションごとに専用の統合ブランチを切るために使う
- `${current_branch}` — タスク作成時に project リポジトリの daemon の HEAD ブランチに解決される

**省略時の解決優先順位:**

1. `parent_id` あり (child task): 親タスクの `base_branch` をそのまま継承。 template 展開は行わない
2. `parent_id` なし + `base_branch` 省略 (root task): 作成時点の `${current_branch}` に展開してから row に保存。 detached HEAD の場合は 400 エラー
3. `parent_id` なし + `base_branch` 指定: template 展開 (`${TASK_REMOTE_ID}` / `${current_branch}`) を行う

エンドツーエンドの例 ([ワークフロー 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) は [docs/workflows.md](../../workflows.md) を参照。

`worktree: true` の挙動については [概念 / worktree](../guide/concepts.md#worktree) を参照してください。

### タスク種別と worktree HEAD

`worktree: true` のプロジェクトでは、 タスク種別によって HEAD branch と fork 元が異なります:

| タスク種別 | HEAD branch | fork 元 | readonly |
|---|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | n/a | sup=true / exec=false |
| **child sup / child exec** | `boid/<task_id8>` | **親タスクの HEAD branch** | sup=true / exec=false |

- **root タスク** (`parent_id == ""`): `base_branch` を HEAD に直接乗る。 `base_branch` が project HEAD と一致する場合 (case 1) は worktree を持たず project root 上で動く。 不一致の場合 (case 2/3) は `base_branch` を HEAD とした専用 worktree が割り当てられる
- **child タスク** (親あり): 常に `boid/<task_id8>` branch の worktree を持つ。 fork 元は **親タスクの HEAD branch** (親が root なら `base_branch`、 親が child なら `boid/<parent_id8>`)。 直接の親のみを参照する (1 hop)
- `task.BaseBranch` は PR target として全子タスクに継承され、 `BOID_BASE_BRANCH` env で executor に渡る

### HEAD branch ロック (1 project × 1 HEAD branch)

同一 worktree で複数タスクを同時実行しないため、 **`<projectID>:<HEAD branch>`** を単位としたロックを保持します:

| タスク種別 | HEAD branch | lock key |
|---|---|---|
| root sup / root exec | `task.BaseBranch` | `<projectID>:<baseBranch>` |
| child sup / child exec | `boid/<task_id8>` | `<projectID>:boid/<task_id8>` |

- **直列実行**: 同 project で同 `base_branch` の root タスク 2 つが executing 遷移すると、 後発は前者の完了 (terminal 遷移) まで FIFO キューで待つ
- **並行 OK**: 異なる `base_branch` の root 同士、 root + child 、 異なる child 同士は並行実行可能
- lock は executing 中ずっと保持 (awaiting 中も保持)。 release は terminal 遷移時のみ
- task 作成時には validate しない。 executing 遷移時に acquire を試みる

### 依存子の最新化とマージ責務

boid コアは子タスクの dispatch 順序や base 同期には関与しません。 sub-sup (子 supervisor) が子タスクの dispatch 順序と base 同期を制御します:

```
A (executor) が done → A の PR を merge
                         ↓
            sub sup が git fetch && merge で自 branch (boid/<subid8>) を更新
                         ↓
            sub sup が B を dispatch → B の worktree が更新済み boid/<subid8> から fork
```

merge のタイミング・コマンド・対象は **project 側 instruction の責務** であり、 skill / boid コアには記述しません。 boid コアの関与は `BOID_BASE_BRANCH` / `BOID_PARENT_BRANCH` env を渡すことに限定されます。

### `default_instruction`

単一の Instruction object です。 タスク作成時に Task.Instructions 配列に append され、 これが executing で agent に渡される最初の active instruction になります。

reopen 時は `boid task reopen <id> --message "..."` で新しい Instruction を append し、 配列の最後の要素 (= 直近の active instruction) が agent に渡されます。 `agent` / `model` / `interactive` は前回 active から継承されます。

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
    stdin: false
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
| `stdin` | bool | このコマンドへ標準入力を渡してよいか |
| `path` | string | バイナリの絶対パス (host の `$PATH` 解決を上書きしたい場合) |
| `env` | map (string → string) | このコマンド呼び出し時に追加する環境変数 |

特殊な使い方として、 `path` に kit / プロジェクト内の相対パスを書くと、その path のコマンドだけがサンドボックスから host へ流れます (例: `path: e2e/run.sh`)。

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
  # gitignored だが worktree でも参照させたいファイル (例: .NET の global.json)
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
- `${WORKTREE}` — タスクが実行されるサンドボックスの cwd。 `worktree: true` の task では worktree path に、 `worktree: false` の task では `${PROJECT_WORKDIR}` と同じ値に解決される

主な用途は、 `.gitignore` してあるが worktree からも参照させたいファイル (`.NET` の `global.json`、 `.env.local`、 `appsettings.Development.json` など) をホストの project workdir から worktree に bind することです。

`target` を **明示** し、 展開後 `source` と等値になった binding は self-mount を避けるため自動的に skip されます。 上の例の binding は:

- `worktree: true` の task では `/host/proj/global.json` → `/runtime/.../<task>/global.json` に bind され、
- `worktree: false` の task では同一 path に潰れて skip される (project ディレクトリは既に projectVisibilityMounts でサンドボックスに見えているため不要)

ので、 同じ宣言で worktree モードに依存せず動作します。

### Instruction

`default_instruction` に書く構造体です。

```yaml
default_instruction:
  type: execution
  agent: claude-code
  model: sonnet
  message: |
    ...
```

| キー | 型 | 役割 |
|---|---|---|
| `type` | enum | `execution` のみ (旧 `rework` / `verification` は廃止) |
| `agent` | string | この instruction を受け取る kit の identifier (例: `claude-code`) |
| `name` | string | 同じ agent に複数 instruction を渡す場合の識別子 (省略可) |
| `message` | string | agent に渡される指示文 |
| `interactive` | bool | `true` で agent を対話的なセッションとして起動する (kit 側がサポートしていれば) |
| `model` | string | agent が選ぶモデル名 (例: `opus`、 `sonnet`)。 kit 側で解釈される |

### CommandSpec

`commands` 配下のエントリはサンドボックス内から `boid exec <name>` で呼び出せる名前付きコマンドを宣言します。

```yaml
commands:
  shell:
    command: [bash]
  test:
    command: [go, test, "./..."]
    readonly: false
```

| キー | 型 | 役割 |
|---|---|---|
| `command` | string のリスト | 実行する argv。 `${VAR}` 形式の環境変数は読み込み時に展開される |
| `readonly` | bool | このコマンド単発でサンドボックスを読み取り専用にしたい場合に `true` |

## プロジェクトローカル設定 (`.boid/project.local.yaml`)

`project.yaml` と並んで、 `.boid/project.local.yaml` を置くと一部フィールドをローカルでだけ上書きできます。 `git ignore` する前提で、共有しない設定 (個人の host_commands など) を入れる場所です。

サポートされるフィールド:

```yaml
version: 1
host_commands:
  ...
additional_bindings:
  ...
env:
  ...
secret_namespace: ...
```

`task_behaviors` や `kits` はここで上書きできません。

## 例: 実プロジェクトの構成

`boid` 自身のリポジトリにある `.boid/project.yaml` (抜粋) を載せておきます。 2 つの behavior (`supervisor`, `executor`) を定義し、 project トップで `worktree: true` を宣言することで executor タスクが専用の git worktree を持つ構成です。

```yaml
id: boid
name: boid

# Project トップの worktree フラグ: タスク種別に応じて worktree を割り当てる。
# root タスクの HEAD = base_branch (case 1 = project root、 case 2/3 = worktree)。
# child タスクは常に boid/<id8> worktree を持つ。
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

commands:
  shell:
    command: [bash]

task_behaviors:
  executor:
    name: executor
    default_instruction: { ... }
  supervisor:
    name: Supervisor
    default_instruction: { ... }
```

このスキーマで作れる 3 種類のワークフローの例は [ワークフロー](../../workflows.md) を参照してください。
