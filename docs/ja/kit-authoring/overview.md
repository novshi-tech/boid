# Kit 作者向け 概要

`kit.yaml` というファイル形式についての最小限のガイドです。

> **退役のお知らせ (Phase 2.5 PR6、2026-07)**: kit という**機構**そのものが退役しました。`boid kit init` / `list` / `remove` と `boid workspace configure` は撤去済みで、 kit を発見・インストール・管理する CLI はもう残っていません。 このページが説明しているのは `kit.yaml` という**ファイル形式だけ**であり、 これは workspace の (legacy な) `kits: [...]` リストが参照している場合に限り、 `boid workspace create/edit/import` または `boid project migrate` の実行時に一度だけ `host_commands` / `env` / `additional_bindings` へ展開 ("materialize") されます。 その `kits:` リスト自体も Phase 2.5 PR7 で撤去予定の legacy field です (`docs/plans/workspace-db-consolidation.md` 参照)。 **新規に設定したい場合は kit を書かず、 `boid workspace create`/`edit`/`import` で `host_commands` / `env` / `additional_bindings` を workspace に直接書いてください** ([オンボーディング](../guide/onboarding.md) 参照)。 このページ (および `kit.yaml` を手で書くこと自体) が意味を持つのは、 既存の kit 参照 workspace を保守する場合か、 他人が書いた kit を読む場合だけです。
>
> **kit は hooks も task behavior も提供しません。** これは PR6 以前から既にそうでした — hooks は常に `project.yaml` の関心事 (`task_behaviors.<name>.hooks`) であり、 kit が提供するものだったことは一度もありません。 `hooks:` / `commands:` / `detect:` / `requires:` / `provides_agent:` を含む古い kit.yaml を見ている場合は、 下の [もう読まれないフィールド](#もう読まれないフィールド-kit-init-退役以前) を参照してください — 現行のローダーはこれらを全て無視します。

「kit が何か」 は [概念](../guide/concepts.md#kit) で定義済みです。

## ディスク上のレイアウト

kit は `kit.yaml` を含むディレクトリです。ローダーはこのファイル以外を読みません:

```
my-kit/
└── kit.yaml
```

複数の kit を 1 リポジトリに同居させる場合は、 リポジトリ直下にサブディレクトリを並べてそれぞれを kit にします (公式 [boid-kits](https://github.com/novshi-tech/boid-kits) はこの形ですが、 中身の大半は PR6 以前のもので、 もう読まれないフィールドのサンプルになっています — 下記参照)。

## 現行ローダーが読む `kit.yaml` フィールド

```yaml
meta:
  name: My kit
  description: One-line description shown in UIs
  category: workflow            # language / vcs / ci / agent / workflow / utility

host_commands:                  # (任意) サンドボックスから host へ流すコマンド
  gh:
    allow: [pr, issue]
    env:
      GH_REPO: ${boid:repo_slug}  # host command は中立 cwd で動くので repo 文脈は env で渡す
    reject:
      - match: "*--body-file*"    # サンドボックスのファイルパスは host から見えない
        reason: 'サンドボックスのファイルパスは host から見えない。--body "$(cat <file>)" で内容を渡す'

additional_bindings:            # (任意) サンドボックスへの追加マウント
  - source: ${HOME}/.config/my-tool

env:                            # (任意) サンドボックス内環境変数
  MY_TOOL_FLAG: "1"
```

`meta` / `host_commands` / `additional_bindings` / `env` が、 現行ローダー (`orchestrator.KitMeta`) が理解する**唯一の** 4 つの top-level key です。 それ以外のキーは黙って無視されます — エラーにはならず、 単にサンドボックスに届かないだけです。

各フィールドの細かい仕様は [`project.yaml` リファレンス](../reference/project-yaml.md) で説明している共通ビルディングブロック (`HostCommands`、 `BindMount`) と同じ形を使います。

### `meta`

UIs で kit を識別するためのラベル。 `category` は `language` / `vcs` / `ci` / `agent` / `workflow` / `utility` のいずれかが慣例。

### `host_commands` / `additional_bindings` / `env`

参照元 workspace への materialize 時にマージされます (kit 側の値がデフォルトで、 workspace 自身の値が競合時に勝つ)。 フィールド単位の仕様は [`project.yaml` リファレンス / HostCommands](../reference/project-yaml.md#hostcommands) と [BindMount](../reference/project-yaml.md#bindmount) を参照してください。

## もう読まれないフィールド (kit init 退役以前)

以下のキーは Phase 2.5 PR6 以前に書かれた kit (`boid-kits` にある現行の大半のサンプルも含む) に見られるものです。 既存の `kit.yaml` に残っていても無害です (ローダーが単に無視するだけ) が、 新規に書く kit には追加しないでください。

| フィールド | かつての役割 |
|---|---|
| `detect.script` | `boid kit init` の対話フローが「この kit をこの project に自動選択するか」を判定するために実行していた POSIX sh スクリプト。選択フロー自体がもう存在しません。 |
| `requires.commands` | kit が動くために PATH 上に必要だった host コマンド。`boid kit init` 時にチェックされていました。 |
| `provides_agent` | この kit の hook がどの agent 名の instruction を引き取るかの宣言。 kit が hooks / instruction routing を提供しなくなった時点で意味を失いました。 |
| `hooks` | hook 定義 (`id` / `kind: agent` / `agent` / `traits`)。 kit が hook の**発火**そのものを持ったことは実はありません — hooks は `project.yaml` の `task_behaviors.<name>.hooks` で定義するのが権威であり、 現行 (project.yaml レベル) の hook 契約は [`project.yaml` リファレンス](../reference/project-yaml.md) と [Hook スクリプトプロトコル リファレンス](../reference/hook-contract.md) を参照してください。 |
| `commands` | `boid exec` から呼べる名前付きコマンド。 Phase 3-d で廃止済み。 代わりに `boid exec -p <project> -- <argv...>` を使ってください。 |

## 配布

`boid kit install` というコマンドは存在しません (そもそも一度も存在したことがありません — kit 関連で存在したことがあるコマンドは `boid kit init` / `list` / `remove` のみで、 これらも撤去済みです)。 `kit.yaml` を含む kit ディレクトリを `~/.local/share/boid/kits/<name>/` に手で置いてください (そこへ `git clone` する、 あるいはコピーする)。 その上で workspace の legacy な `kits: [...]` リストから `<name>` を参照します。 展開は `boid workspace create/edit/import` または `boid project migrate` 実行時に一度だけ行われます — dispatch のたびにライブで読まれるわけではありません。

それでも kit リポジトリを保守する場合の慣例:

- README に何の kit か / 必要な前提コマンド を簡潔に書く
- リポジトリ直下に複数 kit を同居させる場合、 各サブディレクトリの README を整える
- `meta.category` は実態に合わせる

## 関連ドキュメント

- [概念](../guide/concepts.md) — hook / kit / trait の意味
- [`project.yaml` リファレンス](../reference/project-yaml.md) — 現行の権威ある hooks スキーマ (`task_behaviors.<name>.hooks`) と共通ビルディングブロック (`HostCommands`/`BindMount`)
- [オンボーディング / kit 機構の退役について](../guide/onboarding.md#kit-機構の退役について) — `boid kit init` / `boid workspace configure` の代替
- [状態機械](../guide/state-machine.md) — hook がどのタイミングで発火するか
