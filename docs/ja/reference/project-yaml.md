# `project.yaml` リファレンス

プロジェクトのルートに置く `.boid/project.yaml` の全フィールドをまとめたリファレンスです。

このページは仕様の網羅を目的にしています。 用語の解説は [概念](../guide/concepts.md) を、 動かし方は [Getting started](../getting-started/) を参照してください。

## 役割と配置

- パス: プロジェクトルート直下の `.boid/project.yaml`
- 役割: そのディレクトリを `boid` プロジェクトとして登録し、タスクの種類 (behavior) と各 behavior が使う拡張パッケージ (kit) を宣言する
- 登録: `boid project add <project-root>` で `boid` の DB に取り込まれる
- 変更後の反映: `boid project reload` で再読み込みする

## 最小例

```yaml
id: demo
name: Demo
task_behaviors:
  hello:
    name: Hello
    readonly: true
```

## トップレベルのフィールド

| キー | 型 | 必須 | 役割 |
|---|---|---|---|
| `id` | string | はい | `boid` 内でプロジェクトを一意に識別する文字列。タスク作成時に `project_id` で参照される |
| `name` | string | はい | UI で表示するプロジェクト名 |
| `kits` | KitRef のリスト | いいえ | プロジェクト全体で読み込む kit。 全 behavior で共通に使われる |
| `task_behaviors` | map (string → TaskBehavior) | はい | このプロジェクトで作れる「タスクの種類」一覧 |
| `commands` | map (string → CommandSpec) | いいえ | サンドボックス内から `boid exec` 経由で呼べる名前付きコマンド |
| `host_commands` | HostCommands | いいえ | サンドボックスから host へ流せる外部コマンドの宣言 |
| `additional_bindings` | BindMount のリスト | いいえ | サンドボックスにマウントしたい追加パス |
| `env` | map (string → string) | いいえ | サンドボックス内に流す環境変数 |
| `secret_namespace` | string | いいえ | このプロジェクトの secret を解決する際のネームスペース |

## `task_behaviors.<name>`

map のキーが behavior の識別子 (例: `dev`, `plan`) で、タスク作成時に `behavior:` で指定する名前です。値は次のフィールドを持ちます。

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `name` | string | キー名 | UI 表示用のラベル (省略可) |
| `traits` | string のリスト | (空) | この behavior のタスクが扱う payload trait の宣言 (例: `[tasks]`) |
| `readonly` | bool | `false` | `true` にするとサンドボックスを読み取り専用にする |
| `worktree` | bool | `false` | `true` にするとタスクごとに専用の git worktree を作る |
| `branch_prefix` | string | `boid/` | worktree 用に作るブランチ名のプレフィックス |
| `base_branch` | string | リポジトリの既定ブランチ | worktree のベースとして使うブランチ |
| `default_instructions` | map (role → Instruction) | (空) | 各役割 (`main`, `rework` など) で agent に渡す instruction の雛形 |
| `default_payload` | YAML/JSON | (空) | この behavior でタスクを作ったときの初期 payload |
| `kits` | KitRef のリスト | (空) | この behavior 専用の追加 kit |

`worktree: true` の挙動については [概念 / worktree](../guide/concepts.md#worktree) を参照してください。

### `default_instructions.<role>`

`role` は文字列 (例: `main`、 `rework`) で、 `boid` がどの状態で agent に渡すかを切り替える鍵になります。値は **Instruction** 型 (後述)。 慣例として:

- `main` — `executing` で agent に渡す主指示
- `rework` — `reworking` で agent に渡す修正指示

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
  `as` で alias を付けると、別の kit と consumer 名が衝突するときに区別できます

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
```

| キー | 型 | 既定 | 役割 |
|---|---|---|---|
| `source` | string | (必須) | host 側のパス。 `${HOME}` 等の展開可 |
| `target` | string | `source` と同じ | サンドボックス内のマウント先パス |
| `mode` | string | `""` (ro) | `rw` で読み書き可。空文字列なら読み取り専用 |
| `is_file` | bool | `false` | source がファイルの場合 `true` |
| `optional` | bool | `false` | host に source が無くてもエラーにせずスキップする |

### Instruction

`default_instructions.<role>` に書く構造体です。

```yaml
default_instructions:
  main:
    type: execution
    consumer: claude-code
    model: sonnet
    message: |
      ...
  rework:
    type: rework
    consumer: claude-code
    message: |
      ...
```

| キー | 型 | 役割 |
|---|---|---|
| `type` | enum | `execution` / `rework` / `verification` のいずれか |
| `consumer` | string | この instruction を受け取る kit の identifier (例: `claude-code`) |
| `name` | string | 同じ consumer に複数 instruction を渡す場合の識別子 (省略可) |
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

`boid` 自身のリポジトリにある `.boid/project.yaml` (抜粋) を載せておきます。 `dev` / `plan` / `auto_plan` の 3 つの behavior を定義し、 `worktree: true` の `dev` で AI エージェントによる開発タスクを回しています。

```yaml
id: boid
name: boid

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
  claude:
    command: [claude, --permission-mode, bypassPermissions, ...]
  shell:
    command: [bash]

task_behaviors:
  dev:
    name: dev
    worktree: true
    kits:
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instructions:
      main: { ... }
      rework: { ... }
  plan:
    name: Plan
    readonly: true
    traits: [tasks]
    kits:
      - github.com/novshi-tech/boid-kits/boid-tasks
    default_instructions:
      main: { ... }
```

詳しい例は [4. GitHub PR ベースの開発ワークフロー](../getting-started/04-dev-workflow.md) を参照してください。
