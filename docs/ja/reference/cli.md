# CLI リファレンス

`boid` の全サブコマンドを役割別に一覧したリファレンスです。 各コマンドの詳細フラグは `boid <subcommand> --help` が常に最新です。 このページは「何ができるか」 を 1 ページで眺めるための目次として使ってください。

## 共通

### 起動

引数無しの `boid` はヘルプを表示します。

```bash
boid --help                 # サブコマンド一覧
boid <command> --help       # 個別ヘルプ
```

### グローバルフラグ

| フラグ | 用途 |
|---|---|
| `-o, --output {plain,json,yaml}` | 出力形式 (既定 `plain`)。スクリプト連携には `json` が便利 |

### 自動起動

daemon が止まっているときに以下のコマンドを呼ぶと、自動で `boid start` が実行されます。自動起動をスキップする例外コマンドは `start` / `stop` / `gc` / `check` / `init` / `fetch` / `web set-url` / `web set-addr` / `project migrate` です。手動で起動・停止する必要はありません。

`BOID_NO_AUTOSTART=1` を設定すると自動起動をグローバルに無効化できます。

### コマンドの scope 分類 (remote / local / neutral)

各コマンドは内部で `remote`（daemon の HTTP API だけで完結し、将来リモート daemon に接続しても動作する）/ `local`（daemon lifecycle や CLI プロセス自身が動くホストの filesystem に依存する）/ `neutral`（daemon 接続そのものを必要としない）のいずれかに分類されています（`boid.scope` cobra annotation、全 leaf command が対象で未分類は build failure）。現状はまだこの分類に基づく実行時の接続先チェックは入っていません — Phase 3 (CLI リモート接続) 実装の足場として Phase 2.5 で先行導入されたものです。詳細は `docs/plans/cli-remote-connection.md` を参照。

## サーバライフサイクル

| コマンド | 役割 |
|---|---|
| `boid start [--db-path PATH] [--socket-path PATH] [--kits-dir DIR] [--key-file-path PATH]` | daemon を起動 (子プロセスで detach、自身は即時 return)。HTTP アドレスは `config.yaml` の `web.http_addr` または `boid web set-addr` で設定する |
| `boid stop` | daemon を停止。 PID 指定で kill すると socket が残るのでこちらを使う |
| `boid gc [--older-than DURATION] [--dry-run]` | 古い完了 / abort タスクを GC (daemon が起動時から自動でも回している)。`--dry-run` を付けると削除せずに対象一覧を表示する |
| `boid check` | host の前提コマンドや hook の依存をチェック |
| `boid init [DIR]` | **(廃止)** 廃止ガイダンスを表示。`boid project init\|add` (+ 任意で `boid workspace create/edit/import`) を使ってください。詳細は [オンボーディング](../guide/onboarding.md) を参照 |

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

### `project local` — 廃止

`boid project local ...` コマンドは廃止されました。
`project.local.yaml` が担っていた `host_commands` / `additional_bindings` / `env` は、`workspace.yaml` に集約されます。
`boid project migrate <dir>` で自動変換できます。詳細は [移行ガイド](../guide/migration.md) を参照してください。

## タスク

タスクの作成・観察・修正は `boid task` 配下です。 [概念 / タスク](../guide/concepts.md#タスク-task) と [状態機械](../guide/state-machine.md) も併せて参照してください。

| コマンド | 役割 |
|---|---|
| `boid task list [--status STATUS] [--workspace ID] [--behavior NAME]` | タスク一覧 |
| `boid task create [-f FILE]` | YAML を stdin (または `-f`) で渡してタスクを作成 |
| `boid task show <id> [--field PATH]` | タスク詳細 (status と payload)。 `--field` 指定時は dotted path で 1 フィールドのみ plain text 出力 (例: `--field status`, `--field payload.artifact.report`, `--field awaiting.question`, `--field lifecycle.abort.message`) |
| `boid task watch <id> [--interval DURATION]` | status と payload の変化をライブ表示 |
| `boid task update <id> [-f FILE \| --patch-file FILE] [--payload-file FILE] [--instructions-file FILE]` | タスクを更新。 ファイルパス `-` で stdin。`-f` は `--patch-file` のショートハンド |
| `boid task delete <id> [--force]` | タスク削除 (active 中は `--force` が必要) |
| `boid task duplicate <source_id> [--auto-start]` | 既存タスクを複製 |
| `boid task reopen <id> [-m MSG \| --message MSG]` | done のタスクを executing に戻し、 `--message` で渡した instruction を `Task.Instructions` 配列に append (auto-merge コンフリクト時など)。`-m` は `--message` のショートハンド |
| `boid task rerun <id> [--auto-start] [--instructions-file FILE]` | done / aborted のタスクを pending にリセットして同じ ID で再実行 |
| `boid task notify <id> --message MSG [--ask QUESTION] [--question-id ID] [--done] [--fail] [--progress] [--session-id ID]` | agent からユーザへ通知 (`~/.config/boid/config.yaml` の `notify.command` を起動)。 `--ask` を指定すると Q&A モードになりタスクを `awaiting` に遷移させる |
| `boid task answer --task ID --question-id ID --answer TEXT` | `awaiting` 状態のタスクに回答を送る。 タスクを `awaiting → executing` に遷移させ hook を再起動する |
| `boid task import [-f FILE] [--project ID]` | JSONL からタスクを一括インポート |

notify スクリプトには env で `BOID_TASK_ID` / `BOID_TASK_TITLE` / `BOID_PROJECT_ID` / `BOID_PROJECT_NAME` / `BOID_MESSAGE` / `BOID_TASK_URL` (`web.public_url` 設定時のみ) が渡される。

#### `boid task notify` オプション

| フラグ | 必須 | 説明 |
|---|---|---|
| `--message, -m MSG` | ◎ (`--progress` 以外) | 通知テキスト。 notify スクリプトに `BOID_MESSAGE` として渡される。`--progress` 以外のモードでは必須 |
| `--ask QUESTION` | | 質問テキスト。 指定するとタスクを `awaiting` に遷移させ Q&A モードに入る |
| `--question-id ID` | | Q&A ターンを識別する UUID。省略時は boid が自動生成する |
| `--done` | | 正常完了を通知。 `done_request` ライフサイクルエントリを記録し、ジョブ終了後に daemon がタスクを `done` に遷移させる |
| `--fail` | | 失敗を通知。 `fail_request` ライフサイクルエントリを記録し、ジョブ終了後に daemon がタスクを `aborted` に遷移させる |
| `--progress` | | タイムラインに進捗エントリを記録するのみ (状態変化なし、`--message` は省略可) |
| `--session-id ID` | | この通知を特定のエージェントセッションに紐付ける |

`--ask` / `--done` / `--fail` / `--progress` は相互排他。 いずれも指定しない場合は単純な FYI 通知 (状態変化なし)。

```bash
# 単純通知
boid task notify ${BOID_TASK_ID} --message "PR #42 を確認してください"

# Q&A モード (awaiting に遷移)
boid task notify ${BOID_TASK_ID} \
  --message "マージ判断が必要です" \
  --ask "PR #42 をマージしてよいですか？"

# 完了通知 (ジョブ終了後にタスクを done に遷移)
boid task notify ${BOID_TASK_ID} --done --message "完了しました"

# 失敗通知 (ジョブ終了後にタスクを aborted に遷移)
boid task notify ${BOID_TASK_ID} --fail --message "エラーが発生しました"

# 進捗更新 (タイムラインのみ、状態変化なし)
boid task notify ${BOID_TASK_ID} --progress --message "ステップ 2/5 完了"
```

#### `boid task answer` オプション

| フラグ | 必須 | 説明 |
|---|---|---|
| `--task ID` | ◎ | 回答対象のタスク ID |
| `--question-id ID` | ◎ | 回答する Q&A ターンの UUID |
| `--answer TEXT` | ◎ | 回答テキスト |

**終了コード**:
- `0`: 回答を保存し、タスクを `awaiting → executing` に遷移させた
- `1`: タスクが `awaiting` 状態でない、または引数不正

```bash
boid task answer \
  --task 550e8400-e29b-41d4-a716-446655440000 \
  --question-id q-abc-123 \
  --answer "yes"
```

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
```

`behavior_spec` を渡すと `project.yaml` の task_behaviors を参照せず、 inline でタスクの設定を指定できます。

### `task hook` (タスク単位の hook 操作)

| コマンド | 役割 |
|---|---|
| `boid task hook list <task-id> [--status STATUS]` | このタスクの現状で発火する hook 一覧。`--status` で hook ジョブのステータスを絞り込む |
| `boid task hook replay <task-id> <hook-id> [--status STATUS]` | 特定の hook を再実行。`--status` で hook ジョブのステータスを絞り込む |

`boid stop` 等でエージェント hook が中断された場合は、`boid task hook list <task-id>` で再発火可能な hook を確認し、`boid task hook replay <task-id> <hook-id>` で復旧できます。

### タスク観察ヘルパ

| コマンド | 役割 |
|---|---|
| `boid task artifacts <id> [--field PATH] [--output-file FILE]` | `payload.artifact` を整形。`--field` で単一フィールドを抽出、`--output-file` でファイルに書き出す |
| `boid task tree [<id>]` | 親子タスクのツリー表示 |

## アクション

タスクに対する手動遷移を発行します。

```bash
boid action send --task <task-id> --type <action-type> [--payload FILE]
```

主な `<action-type>`: `start` / `done` / `reopen` / `abort`。詳細は [状態機械 / 手動遷移](../guide/state-machine.md#手動遷移) を参照。 reopen で新しい instruction を送るには `boid task reopen <id> --message "..."` を使う方が便利。

## ジョブ

hook の実行記録を扱います。

| コマンド | 役割 |
|---|---|
| `boid job list --task <task-id>` | 指定タスクで動いた全ジョブ |
| `boid job show <job-id>` | 1 ジョブの詳細 (status / exit_code / output 全文) |
| `boid job watch <job-id> [--interval DURATION]` | 終了するまで待つ。`--interval` でポーリング間隔を指定する |
| `boid job log <job-id>` | transcript ログ (実行ストリーム) |
| `boid job done <job-id> [--exit-code N] [--output-file FILE]` | (内部用) ジョブ完了を daemon に通知 |

`boid job done` は通常 sandbox の EXIT trap から呼ばれるもので、 ユーザが直接叩くことは稀です。

## Kit (コマンドとしては廃止)

`boid kit init` / `boid kit list` / `boid kit remove` および `boid workspace configure` は Phase 2.5 PR6 (2026-07) で撤去されました。`env` / `additional_bindings` は現在 [Workspace](#workspace) の CLI で workspace に直接設定します。`host_commands` はこれらとは違う二層構造です — workspace が持つのは参照名の `[]string` (`host_commands: [gh, aws]`) だけで、実際の定義 (`path` / `allow` / `deny` / `env`) は daemon 側の `~/.config/boid/host_commands.yaml` に集約管理されています。`kit init` が無くなった今どうやってこのファイルを埋めるかは、下記の [Host Commands](#host-commands)（または [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ)）を参照してください。

`kit.yaml` 自体のフォーマットは無くなっていません (手で `kit.yaml` を書いて配置する運用は引き続き可能)。 ただし Phase 2.5 PR7 で `WorkspaceMeta.Kits` フィールドがコードから完全撤去され、 `boid workspace create/edit/import` に `kits:` を直接渡す経路は reject されるようになりました。 残っているのは `boid workspace assign` の auto-create 補助 (legacy shadow yaml の `kits:` をクライアント側で一度だけ解決) と、 `boid project migrate` が生成する legacy kit (host_commands/additional_bindings を workspace に直接畳み込み) の 2 経路のみです。フォーマットの詳細は [Kit 作者向け概要](../kit-authoring/overview.md) を、退役の経緯は [オンボーディング / kit 機構の退役について](../guide/onboarding.md#kit-機構の退役について) を参照してください。

## Web

[Web UI](../guide/web-ui.md) のデバイス認証を管理します。

| コマンド | 役割 |
|---|---|
| `boid web pair [--label LABEL]` | 5 分有効・単回使用のペアリングコードを発行。`--label` で新デバイスに人が読める名前を付ける |
| `boid web devices` | ペアリング済みデバイス一覧 |
| `boid web revoke <id>` | 特定デバイスを失効 |
| `boid web revoke-all` | 全デバイスを失効 |
| `boid web set-url <URL>` | 公開 URL を `config.yaml` に書き込み (マジックリンクのレンダリングに使う) |
| `boid web set-addr <ADDR>` | HTTP リッスンアドレスを `config.yaml` に書き込む (例: `boid web set-addr :9090`)。次回 daemon 起動時に反映される |

## Secret

API トークン等を暗号化して保存します。鍵は `~/.local/share/boid/secret.key`。

| コマンド | 役割 |
|---|---|
| `boid secret set <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 値を保存 (stdin から、または対話プロンプト) |
| `boid secret get <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 値を取得 |
| `boid secret list [-n NAMESPACE \| --namespace NAMESPACE]` | キー一覧 |
| `boid secret delete <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 削除 |

## Workspace

project の実行環境 (`host_commands` / `env` / `capabilities` / `allowed_domains` / `additional_bindings`) を machine 単位でまとめる機能です。`workspaces` テーブルで DB 管理され (Phase 2.5)、`default` workspace は daemon 起動時に常に自動生成されます。project を登録すると自動的に `default` に割り当てられ、`boid project init/add --workspace <slug>` は get-or-create (未知の slug でも空の workspace を自動作成してから紐付け)。

| コマンド | 役割 |
|---|---|
| `boid workspace list` | ワークスペース一覧 |
| `boid workspace show <slug>` | 定義 (host_commands/env/capabilities) と割り当て済み project を表示 |
| `boid workspace create <slug> [--from-file <yaml>]` | 新規作成 (`--from-file` 省略時は空の workspace) |
| `boid workspace edit <slug> --from-file <yaml>` | 既存 workspace を丸ごと置き換え (自動 If-Match、`--force` で last-write-wins) |
| `boid workspace import <file> [--mode create-only\|replace] [--slug SLUG]` | yaml ファイルから取り込み。`--mode` 省略時は `create-only` (既存 slug には 409) |
| `boid workspace export <slug> [--output FILE]` | 定義を yaml として書き出す (省略時 stdout) |
| `boid workspace assign <project-ref> <workspace-id>` | プロジェクトをワークスペースに紐付け (未知の slug は 404。ただしローカル `workspace.yaml` が存在すればそこから auto-create) |
| `boid workspace clear <project-ref>` | プロジェクトの紐付けを `default` にリセット |
| `boid workspace remove <slug>` | ワークスペースを削除 (割り当て済み project は `default` へ再割当。`default` 自体は削除不可) |

## Host Commands

daemon が集約する `~/.config/boid/host_commands.yaml` (workspace 群の `host_commands` を preflight 時に集約した設定) を確認・再読込します (Phase 2.5 PR4)。

| コマンド | 役割 |
|---|---|
| `boid host-commands list` | daemon が把握している host_commands の名前一覧 |
| `boid host-commands reload` | `host_commands.yaml` を手で編集した後に daemon に再読込させる |

## サンドボックス操作

| コマンド | 役割 |
|---|---|
| `boid exec -p <project-ref> [--name NAME] [--readonly] -- <argv...>` | サンドボックス内で任意の argv を実行。 project の `host_commands` / `env` / `additional_bindings` を継承する。 `--` 以降が sandbox 内の argv (旧 `commands:` 名前指定は Phase 3-d で廃止)。 `--name` でジョブの表示名、 `--readonly` でワークスペースを read-only に |
| `boid attach <job-id>` | 実行中のジョブの runtime に attach (interactive ジョブ向け) |
| `boid fetch <url>` | URL のコンテンツをホスト側で取得して出力する (直接 HTTP アクセスが制限されているサンドボックス内から使用可) |

## エージェント

実行中のエージェントジョブを操作します。

| コマンド | 役割 |
|---|---|
| `boid agent claude  -p <project> [--resume <session-id>] [--instruction "..."] [--readonly] [--model M] [--name NAME] [--no-attach]` | claude セッションをサンドボックス内で起動し PTY に attach する。 `--resume` で既存セッションを再開、 `--no-attach` で job-id だけ表示して終了 |
| `boid agent codex   -p <project> [同上]` | **[実験的]** codex セッションを起動。 `--instruction` なしでは sandbox 内で `codex` TUI を起動、 `--instruction` ありでは `codex exec` (1 ターン smoke) にフォールバック。 セッション永続化・`boid task notify` 連携・usage 計上は未実装 (詳細は `docs/plans/multi-harness-production.md`) |
| `boid agent opencode -p <project> [同上]` | **[実験的]** opencode セッションを起動。 `--instruction` なしでは sandbox 内で `opencode <project>` TUI を起動、 `--instruction` ありでは `opencode run` (1 ターン smoke) にフォールバック。 セッション永続化・`boid task notify` 連携・usage 計上は未実装 (詳細は `docs/plans/multi-harness-production.md`) |
| `boid agent stop <job-id>` | エージェントプロセスに SIGUSR1 を送り、正常停止を要求する |

サンドボックス内で対話シェルを開きたい場合は `boid exec -p <project> -- bash` を使う (`boid agent shell` は git gateway cutover 後に退役)。

## シェル補完

```bash
boid completion bash   # Bash 補完スクリプトを生成
boid completion zsh    # Zsh 補完スクリプトを生成
boid completion fish   # Fish 補完スクリプトを生成
```

シェルプロファイルで source してください (例: `source <(boid completion bash)`)。

## 出力形式

`-o json` を付けるとほぼ全コマンドが JSON を出すので、 `jq` 等での加工に向きます。

```bash
boid task list -o json | jq '.[] | select(.status=="executing")'
boid task show <id> -o yaml
```

## 関連ドキュメント

- [Getting started](../getting-started/) — 順を追ったチュートリアル
- [概念](../guide/concepts.md) — task / job / hook / kit / payload / trait の意味
- [状態機械](../guide/state-machine.md) — 手動遷移と自動遷移のルール
- [`project.yaml` リファレンス](project-yaml.md) — プロジェクト定義のフィールド
- [Hook スクリプトプロトコル](hook-contract.md) — hook の入出力契約
