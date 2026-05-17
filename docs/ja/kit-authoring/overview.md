# Kit 作者向け 概要

このページは、自分で kit を書きたい人向けの最小限のガイドです。

「kit が何か」 は [概念](../guide/concepts.md#hook-と-kit) で定義済みです。 ここでは ディスク上のレイアウト、 `kit.yaml` の主要フィールド、 hook スクリプトのプロトコル、 配布の仕方をまとめます。

## ディスク上のレイアウト

kit は `kit.yaml` を含むディレクトリです。最小例:

```
my-kit/
├── kit.yaml
├── detect.sh         (任意)
└── hooks/
    └── my-hook.sh
```

複数の kit を 1 リポジトリに同居させる場合は、 リポジトリ直下にサブディレクトリを並べてそれぞれを kit にします (公式 [boid-kits](https://github.com/novshi-tech/boid-kits) はこの形)。

## `kit.yaml` の主要フィールド

```yaml
meta:
  name: My kit
  description: One-line description shown in UIs
  category: workflow            # language / vcs / ci / agent / workflow / utility

detect:
  script: detect.sh             # (任意) 適用判定スクリプト

requires:
  commands:                     # (任意) 必要な host コマンド
    - gh

provides_agent: my-agent        # (任意) この kit が受け取る instruction の agent 名

hooks:
  - id: my-hook
    kind: agent                 # (任意) "agent" を付けると instruction routing 対象
    agent: my-agent             # (任意) この hook 宛の instruction を受け取る
    traits:
      consumes: [instructions]
      produces: [artifact]

commands:                       # (任意) サンドボックス内 boid exec 用コマンド
  build:
    command: [make, build]

host_commands:                  # (任意) サンドボックスから host へ流すコマンド
  gh:
    allow: [pr, issue]

additional_bindings:            # (任意) サンドボックスへの追加マウント
  - source: ${HOME}/.config/my-tool

env:                            # (任意) サンドボックス内環境変数
  MY_TOOL_FLAG: "1"
```

各フィールドの細かい仕様は [`project.yaml` リファレンス](../reference/project-yaml.md) で説明している共通ビルディングブロック (`HostCommands`、 `BindMount`、 `Instruction`) と同じ形を使います。

### `meta`

UIs で kit を識別するためのラベル。 `category` は `language` / `vcs` / `ci` / `agent` / `workflow` / `utility` のいずれかが慣例。

### `detect`

`boid init` のようなセットアップフローで、 「このプロジェクトにこの kit が適用できるか」を判定するためのスクリプトです。 POSIX sh で書き、 stdout 1 行目に次の文字列のいずれかを出します。

- `required` — このプロジェクトにとって必須 (自動選択される)
- `optional` — 候補として提示される (デフォルトでは選択されない)
- 空 / その他 — 適用しない

タイムアウトは 5 秒。 スクリプトの実行ディレクトリはプロジェクトルートです。

### `requires.commands`

kit が動くために PATH 上に必要な host コマンド。 インストール時のチェックや UI でのガイドに使われます。

### `provides_agent`

この kit が「どの agent 名で書かれた instruction を引き取るか」を宣言します。 例えば claude-code kit は `provides_agent: claude-code` を設定し、 `default_instruction.agent: claude-code` の instruction を受け取ります。

### `hooks`

詳細は次章のスクリプトプロトコルを参照。 hook は `executing` 状態でのみ走ります。

`traits.consumes` と `traits.produces` は、 hook がどの payload trait を読み書きするかを宣言します。 `consumes` の末尾に `?` を付けると optional (なくてもエラーにしない) になります。

## Hook スクリプトのプロトコル

ここでは概略のみ示します。 完全な仕様 (TaskJSON のフィールド一覧、 環境変数、 `payload_patch.json` ファイル経路など) は [Hook スクリプトプロトコル リファレンス](../reference/hook-contract.md) を参照してください。

### 入力 (stdin)

タスク全体を JSON で受け取ります (TaskJSON)。 代表的なフィールド:

- `id` — タスク ID
- `project_id`、 `behavior`
- `status` — 現在の状態
- `title`、 `description`
- `payload` — 現在の payload
- `instructions` — routed 済みの instruction (`kind: agent` の hook のみ)

加えて環境変数として `BOID_TASK_ID` / `BOID_JOB_ID` / `BOID_PROJECT_ID` などがセットされます。

### 出力 (payload patch)

payload を更新したい場合、 `$HOME/.boid/output/payload_patch.json` に次の形の JSON を書き出します。

```json
{
  "payload_patch": {
    "artifact": { "result": "ok" }
  }
}
```

ファイルがない場合に限り、 stdout に書いた JSON が同じ役割をします (フォールバック)。 新規 hook ではファイル経路を推奨します — agent 系 hook は agent の出力が stdout に混じるため、 ファイル経路で誤認を避けられます。

`payload_patch` の中身は payload に対する JSON merge 指示です。 ネストしたキーをそのまま書けば、 その階層だけが更新されます。 何も更新しない場合は何も書かなくて構いません。

### ログ (stderr)

進捗ログ、 エラーメッセージなどは stderr に出します。 `boid job show <job-id>` で見られるので、 デバッグに役立つ情報を遠慮せず吐いてください。

### 終了コード

- `0` — 成功
- 非 0 — 失敗。 タスクは `aborted` にはならず、 `boid` がジョブを `failed` 扱いにする (再実行されるかは状態機械の自動遷移次第)

## 配布

kit は別リポジトリで配布するのが標準です。 `boid kit install <git-host>/<owner>/<repo>` がそのリポジトリを `~/.local/share/boid/kits/<git-host>/<owner>/<repo>/` に `git clone` します。 ユーザは `project.yaml` の `kits:` フィールドで `<git-host>/<owner>/<repo>/<sub-path>` の形で参照します。

公開のための慣例:

- README に何の kit か / どの agent の instruction を受け取るか / 必要な前提コマンド を簡潔に書く
- リポジトリ直下に複数 kit を同居させる場合、 各サブディレクトリの README を整える
- `meta.category` は実態に合わせる
- `requires.commands` は省略せず宣言する (ユーザの初期セットアップに直結する)

## 参考実装

- [`github.com/novshi-tech/boid-kits`](https://github.com/novshi-tech/boid-kits) — 公式 kit 群。 `claude-code`、 `github-cli`、 `go-dev` などが多様な構成のサンプルとして読みやすい

## 関連ドキュメント

- [概念](../guide/concepts.md) — hook / kit / trait の意味
- [`project.yaml` リファレンス](../reference/project-yaml.md) — kit が project.yaml からどう参照されるか
- [状態機械](../guide/state-machine.md) — hook がどのタイミングで発火するか
