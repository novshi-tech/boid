# CLI リファレンス

`boid` の全サブコマンドを役割別に一覧したリファレンスです。 各コマンドの詳細フラグは `boid <subcommand> --help` が常に最新です。 このページは「何ができるか」 を 1 ページで眺めるための目次として使ってください。

## 共通

### 起動

引数無しの `boid` は TUI を起動します。

```bash
boid                        # TUI 起動
boid --help                 # サブコマンド一覧
boid <command> --help       # 個別ヘルプ
```

### グローバルフラグ

| フラグ | 用途 |
|---|---|
| `-o, --output {plain,json,yaml}` | 出力形式 (既定 `plain`)。スクリプト連携には `json` が便利 |

### 自動起動

daemon が止まっているときに以下のコマンドを呼ぶと、自動で `boid start` が実行されます (例外: `start` / `stop` / `gc`)。手動で起動・停止する必要はありません。

## サーバライフサイクル

| コマンド | 役割 |
|---|---|
| `boid start [--http-addr ADDR] [--db-path PATH] [--socket-path PATH] [--kits-dir DIR] [--key-file-path PATH]` | daemon を起動 (子プロセスで detach、自身は即時 return) |
| `boid stop` | daemon を停止。 PID 指定で kill すると socket が残るのでこちらを使う |
| `boid gc [--older-than DURATION]` | 古い完了 / abort タスクを GC (daemon が起動時から自動でも回している) |
| `boid check` | host の前提コマンドや hook の依存をチェック |
| `boid init [DIR]` | 対話形式で新しいプロジェクトをセットアップ |

詳細は [Getting started / インストール](../getting-started/01-install.md) を参照。

## プロジェクト

[`project.yaml` リファレンス](project-yaml.md) の登録 / 管理を行います。

| コマンド | 役割 |
|---|---|
| `boid project add <dir>` | `<dir>/.boid/project.yaml` を `boid` に登録 |
| `boid project list` | 登録済みプロジェクト一覧 |
| `boid project show <ref>` | プロジェクト詳細 (id 完全一致 / 名前部分一致のいずれも可) |
| `boid project remove <ref>` | プロジェクトを登録解除 |
| `boid project reload` | すべてのプロジェクトの `project.yaml` を再読み込み |
| `boid project behaviors <ref>` | そのプロジェクトの task_behaviors 一覧 |

### `project local` (`.boid/project.local.yaml` の編集)

`gitignore` する前提のローカル上書きファイル。 `host_commands` / `additional_bindings` / `env` などをチームと共有せずに付加できます。

| コマンド | 役割 |
|---|---|
| `boid project local init [DIR]` | 空の `project.local.yaml` を作成 |
| `boid project local show [DIR]` | 内容を表示 |
| `boid project local set-env <key> <value> [DIR]` | env override を追加 |
| `boid project local unset-env <key> [DIR]` | env override を削除 |
| `boid project local add-binding <path> [DIR]` | additional_bindings 追加 |
| `boid project local remove-binding <path> [DIR]` | additional_bindings 削除 |

## タスク

タスクの作成・観察・修正は `boid task` 配下です。 [概念 / タスク](../guide/concepts.md#タスク-task) と [状態機械](../guide/state-machine.md) も併せて参照してください。

| コマンド | 役割 |
|---|---|
| `boid task list [--status STATUS] [--workspace ID] [--behavior NAME] [--has-depends-on \| --no-depends-on]` | タスク一覧 |
| `boid task create [-f FILE]` | YAML を stdin (または `-f`) で渡してタスクを作成 |
| `boid task show <id>` | タスク詳細 (status と payload) |
| `boid task watch <id> [--interval DURATION]` | status と payload の変化をライブ表示 |
| `boid task get <id> --field <name>` | 特定フィールドのみ取得 (例: `--field title`) |
| `boid task update <id> [--patch-file FILE] [--payload-file FILE] [--instructions-file FILE]` | タスクを更新。 ファイルパス `-` で stdin |
| `boid task delete <id> [--force]` | タスク削除 (active 中は `--force` が必要) |
| `boid task duplicate <source_id> [--auto-start]` | 既存タスクを複製 |
| `boid task reopen <id> [--message MSG]` | done のタスクを executing に戻し、 `--message` で渡した instruction を `Task.Instructions` 配列に append (auto-merge コンフリクト時など) |
| `boid task rerun <id> [--auto-start] [--instructions-file FILE]` | done / aborted のタスクを pending にリセットして同じ ID で再実行 |
| `boid task notify <id> --message MSG` | agent からユーザへ通知 (`~/.config/boid/config.yaml` の `notify.command` を起動)。 boid-plan SKILL の supervisor mode で plan 承認や hard cap 到達時にユーザへ判断を仰ぐ用途 |
| `boid task import [-f FILE] [--project ID] [--datasource ID]` | JSONL からタスクを一括インポート |

notify スクリプトには env で `BOID_TASK_ID` / `BOID_TASK_TITLE` / `BOID_PROJECT_ID` / `BOID_PROJECT_NAME` / `BOID_MESSAGE` / `BOID_TASK_URL` (`web.public_url` 設定時のみ) が渡される。

### `task create` の入力

YAML schema:

```yaml
project_id: <id>
title: <string>
behavior: <name>            # または behavior_spec
auto_start: false
description: ...
payload:    { ... }
instructions: { ... }
depends_on:  [<task-id>, ...]
depends_on_payload: <expr>
```

`behavior_spec` を渡すと `project.yaml` の task_behaviors を参照せず、 inline でタスクの設定を指定できます。

### `task gate` (タスク単位の gate 操作)

| コマンド | 役割 |
|---|---|
| `boid task gate list <task-id>` | このタスクの現状で発火する gate 一覧 |
| `boid task gate replay <task-id> <gate-id>` | 特定の gate を再実行 |

### `task hook` (タスク単位の hook 操作)

| コマンド | 役割 |
|---|---|
| `boid task hook list <task-id>` | このタスクの現状で発火する hook 一覧 |
| `boid task hook replay <task-id> <hook-id>` | 特定の hook を再実行 |

`boid stop` 等でエージェント hook が中断された場合は、`boid task hook list <task-id>` で再発火可能な hook を確認し、`boid task hook replay <task-id> <hook-id>` で復旧できます。

### タスク観察ヘルパ

| コマンド | 役割 |
|---|---|
| `boid task artifacts <id>` | `payload.artifact` を整形 |
| `boid task tree [<id>]` | 親子タスクのツリー表示 |

## アクション

タスクに対する手動遷移を発行します。

```bash
boid action send --task <task-id> --type <action-type> [--payload FILE]
```

主な `<action-type>`: `start` / `done` / `reopen` / `abort`。詳細は [状態機械 / 手動遷移](../guide/state-machine.md#手動遷移) を参照。 reopen で新しい instruction を送るには `boid task reopen <id> --message "..."` を使う方が便利。

## ジョブ

handler の実行記録を扱います。

| コマンド | 役割 |
|---|---|
| `boid job list --task <task-id>` | 指定タスクで動いた全ジョブ |
| `boid job show <job-id>` | 1 ジョブの詳細 (status / exit_code / output 全文) |
| `boid job watch <job-id>` | 終了するまで待つ |
| `boid job log <job-id>` | transcript ログ (実行ストリーム) |
| `boid job done <job-id> [--exit-code N] [--output-file FILE]` | (内部用) ジョブ完了を daemon に通知 |

`boid job done` は通常 sandbox の EXIT trap や host gate wrapper から呼ばれるもので、ユーザが直接叩くことは稀です。

## Kit

[拡張パッケージ](../kit-authoring/overview.md) の取得・更新を行います。

| コマンド | 役割 |
|---|---|
| `boid kit install [repo]` | リポジトリを `~/.local/share/boid/kits/` に clone (`git clone`)。 引数省略でカレントプロジェクトの kits 全部 |
| `boid kit list` | インストール済みのリポジトリ一覧 |
| `boid kit update <repo>` | 既存リポジトリを `git pull` |
| `boid kit remove <repo>` | リポジトリを削除 |

## Web

[Web UI](../guide/web-ui.md) のデバイス認証を管理します。

| コマンド | 役割 |
|---|---|
| `boid web pair` | 5 分有効・単回使用のペアリングコードを発行 |
| `boid web devices` | ペアリング済みデバイス一覧 |
| `boid web revoke <id>` | 特定デバイスを失効 |
| `boid web revoke-all` | 全デバイスを失効 |
| `boid web set-url <URL>` | 公開 URL を `config.yaml` に書き込み (マジックリンクのレンダリングに使う) |

## Secret

API トークン等を暗号化して保存します。鍵は `~/.local/share/boid/secret.key`。

| コマンド | 役割 |
|---|---|
| `boid secret set <key>` | 値を保存 (stdin から、または対話プロンプト) |
| `boid secret get <key>` | 値を取得 |
| `boid secret list` | キー一覧 |
| `boid secret delete <key>` | 削除 |

## Workspace

複数プロジェクトをまとめてグルーピングする機能です。

| コマンド | 役割 |
|---|---|
| `boid workspace list` | ワークスペース一覧 |
| `boid workspace show <id>` | プロジェクトと最近のタスクを表示 |
| `boid workspace assign <project-ref> <workspace-id>` | プロジェクトをワークスペースに紐付け |
| `boid workspace clear <project-ref>` | プロジェクトのワークスペース紐付けを解除 |

## サンドボックス操作

| コマンド | 役割 |
|---|---|
| `boid exec -p <project-ref> [command-name]` | プロジェクトの `commands` で定義した名前付きコマンドをサンドボックス内で実行 |
| `boid attach <job-id>` | 実行中のジョブの runtime に attach (interactive ジョブ向け) |

## 出力形式

`-o json` を付けるとほぼ全コマンドが JSON を出すので、 `jq` 等での加工に向きます。

```bash
boid task list -o json | jq '.[] | select(.status=="executing")'
boid task show <id> -o yaml
```

## 関連ドキュメント

- [Getting started](../getting-started/) — 順を追ったチュートリアル
- [概念](../guide/concepts.md) — task / job / hook / gate / kit / payload / trait の意味
- [状態機械](../guide/state-machine.md) — 手動遷移と自動遷移のルール
- [`project.yaml` リファレンス](project-yaml.md) — プロジェクト定義のフィールド
- [Handler スクリプトプロトコル](handler-contract.md) — hook / gate の入出力契約
