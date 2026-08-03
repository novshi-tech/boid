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

daemon が止まっているときに以下のコマンドを呼ぶと、自動で compose スタックが起動します（host mode、下記）。自動起動をスキップする例外コマンドは `start` / `stop` / `gc` / `check` / `init` / `fetch` / `reap` / `project migrate` / `login` / `logout` / `version` です。手動で起動・停止する必要はありません。

`boid web set-url` / `boid web set-addr` は `boid config set web.public_url|web.http_addr` に統合され（穴8, `docs/plans/release-onboarding.md`）、他の scope=`remote` コマンドと同様に自動起動の対象です（従来の「daemon 停止中はスキップ」という例外ではなくなりました）。

`BOID_NO_AUTOSTART=1` を設定すると自動起動をグローバルに無効化できます。

### コマンドの scope 分類 (remote / local / neutral)

各コマンドは内部で `remote`（daemon の HTTP API だけで完結し、将来リモート daemon に接続しても動作する）/ `local`（daemon lifecycle や CLI プロセス自身が動くホストの filesystem に依存する）/ `neutral`（daemon 接続そのものを必要としない）のいずれかに分類されています（`boid.scope` cobra annotation、全 leaf command が対象で未分類は build failure）。現状はまだこの分類に基づく実行時の接続先チェックは入っていません — Phase 3 (CLI リモート接続) 実装の足場として Phase 2.5 で先行導入されたものです。詳細は `docs/plans/cli-remote-connection.md` を参照。

### Host mode（既定、compose backend 向け）

daemon は compose 一本（`scripts/deploy-container.sh`、`build/container/compose.yml` — `docs/plans/release-onboarding.md` 決定2）。**`boid` CLI 自身が daemon container のライフサイクルを管理する「host mode」が既定の動作**です（`docs/plans/volume-only-daemon.md` §論点c、Option 4 設計）。以前存在した `BOID_MODE=container` の opt-in は撤去済みで、設定不要になりました。

scope=`remote` なコマンド（`task list` 等、daemon の HTTP API を叩くもの）を呼ぶと、`boid` は内部で:

1. `~/.config/boid/cli-token`（無ければ生成、0600）を読み込む
2. `http://127.0.0.1:8442/api/cli-token-check` に `Authorization: Bearer <token>` 付きで届くか確認し（届かない、または token が daemon 側と不一致なら）`scripts/deploy-container.sh` を起動（image build + `compose up -d`）してから再確認 — 認証済みの endpoint を叩くのは、daemon が起動していても token が古い（`~/.config/boid/cli-token` を消して作り直した等）ケースを見逃さないため（`/api/health` は無認証なので token 不一致を検知できない）
3. `Authorization: Bearer <token>` を付けて `http://127.0.0.1:8442` へ実コマンドを dispatch

を行います。`boid start`/`stop` などの scope=`local` コマンド（compose lifecycle 機構そのもの）、`login`/`logout` などの scope=`neutral` コマンドは host mode の影響を受けません。`gc` は scope=`remote` ですが `annotationSkipAutostart` が付いており、daemon が unreachable でも自動起動せず即座にエラーで失敗します（`resolveHostModeClientNoAutostart`、`cmd/host.go`）——「daemon を gc するためだけに daemon を起動する」を避けるための挙動で、`BOID_NO_AUTOSTART=1` を明示指定した場合と同じ結果になります。

**`--profile` を明示的に指定した場合は host mode を迂回**し、従来どおり `profiles.Resolve` チェーン（`--profile` > `BOID_PROFILE` > `default_profile` > unix フォールバック）で名前付き profile（リモート https daemon や別の unix socket）に接続します（`docs/plans/release-onboarding.md`「profiles との優先順位」）。`BOID_PROFILE` 環境変数や `default_profile` だけでは迂回しません — その場のコマンドに `--profile` を明示的に付けたときだけが対象です。

`BOID_NO_AUTOSTART=1` を設定すると、daemon が unreachable でも自動起動を試みずエラーで即座に失敗します。

| 環境変数 | 用途 |
|---|---|
| `BOID_COMPOSE_ROOT` | `scripts/deploy-container.sh` を含む boid リポジトリのルートを明示（未設定時は下記の埋め込みアセット・フォールバックへ。cwd からの自動探索は行わない — 未信頼な checkout に単に `cd` しただけで、そこにある `scripts/deploy-container.sh` がユーザー権限で実行される drive-by 経路になるため、codex round-10 review で撤去済み） |

boid リポジトリのチェックアウトが見つからない場合（`/usr/local/bin/boid` を単体インストールし、任意の project ディレクトリから起動する等）は、埋め込み済みの `compose.yml`（`build/container/assets.go`、`go:embed`）を `$XDG_STATE_HOME/boid/compose/` に展開し、`BOID_IMAGE` にこの CLI バイナリ自身のバージョンに対応する image ref（`internal/version.DefaultContainerImage()`）をセットした上で `compose up -d` を実行するフォールバックが働きます（round-2 codex review Major 1、PR4 でローカル image 前提を撤去 — 事前にローカルへ image が存在している必要はなく、`compose up -d` 自身が pull する）。**`DefaultContainerImage()` が実際に返す値は 2 パターンある** (`internal/version/version.go`): CLI バイナリが exact release tag (`vX.Y.Z` ちょうど) でビルドされている場合のみ `ghcr.io/novshi-tech/boid-runner:<そのタグ>` を返し、それ以外 (pseudo-version・`+dirty`・`(devel)` 等、`go install @latest` 以外の経路で入れた大半のケース) は登録済み GHCR ref を持たないローカルタグ `boid-runner:latest` を返す — 後者はレジストリ prefix を持たないため、ローカルに同名 image が無ければ pull は失敗する。`go install github.com/novshi-tech/boid@latest` は通常リリースタグに解決されるため前者に該当するが、保証ではない。image を fresh build できるのはチェックアウトがある場合のみ（`Dockerfile` の build context が `COPY . .` = go source tree 全体のため）。pull 自体が失敗した場合（ネットワーク不通、arch mismatch、上記のローカルタグ未解決等）は明確なエラーで失敗します。

CLI listener のアドレスは `127.0.0.1:8442` 固定（override 不可）。`build/container/compose.yml` の port publish (`127.0.0.1:8442:8442`) と daemon 自身の listener bind の双方に配線されていない override は実質機能しないため（round-2 codex review Major 2）、host 側は override 手段を持たない。

## サーバライフサイクル

| コマンド | 役割 |
|---|---|
| `boid start` | compose スタックを起動 (`docker/podman compose up -d` 相当、`docs/plans/release-onboarding.md` 決定2)。HTTP アドレスは `boid config set web.http_addr <addr>` で設定する。`--foreground`（または compose 自身の daemon service が設定する `BOID_DAEMON_CHILD=1`）を渡すと、この呼び出し自身が daemon プロセスそのものになる（compose の entrypoint 用 — 通常の対話利用では不要） |
| `boid stop` | compose スタックを停止 (`docker/podman compose down` 相当) |
| `boid gc [--older-than DURATION] [--dry-run]` | 古い完了 / abort タスクを GC (daemon が起動時から自動でも回している)。`--dry-run` を付けると削除せずに対象一覧を表示する。出力には workspace home のサイズ一覧も表示される (表示のみ、削除はしない。詳細は [workspace home ガイド](../guide/workspace-home.md#boid-gc-の-workspace-home-表示)) |
| `boid check` | host の前提コマンドや hook の依存をチェック |
| `boid init [DIR]` | **(廃止)** 廃止ガイダンスを表示。`boid project init\|add` (+ 任意で `boid workspace create/edit`) を使ってください。詳細は [オンボーディング](../guide/onboarding.md) を参照 |

詳細は [Getting started / インストール](../getting-started/01-install.md) を参照。

## プロジェクト

[`project.yaml` リファレンス](project-yaml.md) の登録 / 管理を行います。

| コマンド | 役割 |
|---|---|
| `boid project add <git-url> --workspace=<name> [--name=<project-name>]` | git remote URL を登録し、daemon が bare repository として clone する (docs/plans/volume-only-daemon.md §論点a/b)。`--workspace` は必須、`--name` を省略すると URL の最後のパス要素から project 名を derive する。旧来のホストディレクトリ登録フォーム (`boid project add <dir>`) は PR-4 で撤去済み — git URL の形 (明示スキームまたは scp-like `user@host:path`) に一致しない引数はクライアント側で拒否される。 |
| `boid project list` | 登録済みプロジェクト一覧 (status `ready`/`degraded` も表示 — `project fetch` 参照)。サンドボックス内の boid shim からも同名で呼べるが、こちらは daemon 全体ではなく**同一 workspace 内**のプロジェクトのみ (id/name/upstream_url の JSON) を返す — スコープは呼び出し元トークンの `AllowedProjectIDs` で決まり、指定はできない |
| `boid project show <ref>` | プロジェクト詳細 (id 完全一致 / 名前部分一致のいずれも可)。workspace default (task_behaviors / base_branch / fork_point / default_task_behavior) が 1 つでも適用されている場合は 1 行インジケータを表示する。project.yaml 無し project (`url-` 由来 id) では id/name の由来も表示する。`--explain` を付けるとフィールド単位の由来 (project.yaml / workspace default / unset) を表示する (docs/plans/workspace-default-project.md 論点e) |
| `boid project remove <ref>` (alias: `rm`) | プロジェクトを登録解除。project の DB row を削除する唯一の入口 — `boid` は filesystem / remote の観測結果を根拠に自動削除しない (下記 `project fetch` 参照)。git-URL 登録した project の場合は daemon 管理下の bare repository も削除するため、同じ URL/名前で再度 add しても成功する。 |
| `boid project reload` | すべてのプロジェクトの `project.yaml` を再読み込み |
| `boid project fetch <ref>` | git-URL 登録した project の bare repository で `git fetch --all` を実行し project.yaml を再読み込みする。fetch/reload に失敗しても削除はせず `degraded` 状態にする (`project list`/`show` で確認可能) — 復旧は remote に再度到達可能になってから `boid project rm` + `boid project add` |
| `boid project behaviors <ref>` | そのプロジェクトの task_behaviors 一覧。サンドボックス内の boid shim からも同名で呼べる (`boid project behaviors <ref>`) — 出力は JSON 固定、`ref` は同一 workspace 内のプロジェクトのみ解決可能 (`AllowedProjectIDs` でスコープされる) |

### `project local` — 廃止

`boid project local ...` コマンドは廃止されました。
`project.local.yaml` が担っていた `host_commands` / `env` は、workspace (DB 側) に集約されます (`additional_bindings` は Phase 4 PR4 で撤去済み、[workspace home `init.sh`](../guide/workspace-home.md) に移行)。
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

`boid kit init` / `boid kit list` / `boid kit remove` および `boid workspace configure` は Phase 2.5 PR6 (2026-07) で撤去されました。`env` は現在 [Workspace](#workspace) の CLI で workspace に直接設定します (`additional_bindings` は Phase 4 PR4 で撤去済み — [workspace home `init.sh`](../guide/workspace-home.md) を使う)。`host_commands` はこれらとは違う二層構造です — workspace が持つのは参照名の `[]string` (`host_commands: [gh, aws]`) だけで、実際の定義 (`path` / `allow` / `deny` / `env`) は daemon 側の `~/.config/boid/host_commands.yaml` に集約管理されています。`kit init` が無くなった今どうやってこのファイルを埋めるかは、下記の [Host Commands](#host-commands)（または [オンボーディング / host_commands を定義する](../guide/onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ)）を参照してください。

`kit.yaml` 自体のフォーマットは無くなっていません (手で `kit.yaml` を書いて配置する運用は引き続き可能)。 ただし Phase 2.5 PR7 で `WorkspaceMeta.Kits` フィールドがコードから完全撤去され、 `boid workspace create/edit` に `kits:` を直接渡す経路は reject されるようになりました。 残っているのは `boid workspace assign` の auto-create 補助 (legacy shadow yaml の `kits:` をクライアント側で一度だけ解決) と、 `boid project migrate` が生成する legacy kit (host_commands を workspace に直接畳み込み。 legacy kit の `additional_bindings` は Phase 4 PR4 で撤去済みなので無視される) の 2 経路のみです。フォーマットの詳細は [Kit 作者向け概要](../kit-authoring/overview.md) を、退役の経緯は [オンボーディング / kit 機構の退役について](../guide/onboarding.md#kit-機構の退役について) を参照してください。

## Web

[Web UI](../guide/web-ui.md) のデバイス認証を管理します。

| コマンド | 役割 |
|---|---|
| `boid web pair [--label LABEL]` | 5 分有効・単回使用のペアリングコードを発行。`--label` で新デバイスに人が読める名前を付ける |
| `boid web devices` | ペアリング済みデバイス一覧 |
| `boid web revoke <id>` | 特定デバイスを失効 |
| `boid web revoke-all` | 全デバイスを失効 |
| `boid web set-url <URL>` | 公開 URL (`web.public_url`、マジックリンクのレンダリングに使う) を設定。実体は `boid config set web.public_url <URL>` と同じ `POST /api/config/mutate` 呼び出し (穴8 (b)、`docs/plans/release-onboarding.md`) — compose daemon の `boid_state` volume 内の config.yaml に daemon 側が書き込むので、host 側で直接ファイルを編集する必要はない |
| `boid web set-addr <ADDR>` | HTTP リッスンアドレス (`web.http_addr`) を設定 (例: `boid web set-addr :9090`)。同じく `config set` 相当の API 呼び出し。反映には daemon の再起動 (`boid stop && boid start`) が必要。**注意:** これはコンテナ**内部**の bind アドレスであり、標準の compose デプロイでは host 側に公開されるポート (既定 8080) 自体は変わらない — ポート番号を変えると Web UI に到達できなくなる (詳細は [Getting started / 3. Web UI をセットアップする](../getting-started/03-web-ui.md#listen-アドレスを変える-任意)) |

## Secret

API トークン等を暗号化して保存します。鍵は `~/.local/share/boid/secret.key`。

| コマンド | 役割 |
|---|---|
| `boid secret set <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 値を保存 (stdin から、または対話プロンプト) |
| `boid secret get <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 値を取得 |
| `boid secret list [-n NAMESPACE \| --namespace NAMESPACE]` | キー一覧 |
| `boid secret delete <key> [-n NAMESPACE \| --namespace NAMESPACE]` | 削除 |
| `boid secret oauth login <service> [-n NAMESPACE] [--timeout DURATION]` | API gateway の OAuth2 サービスに対して初回認証を行う (device/loopback/manual、`docs/plans/api-gateway.md` §7 参照)。`<service>` は `config.yaml` の `services.<name>` エントリ名 (`oauth_providers` の provider 名ではない) |

## Workspace

project の実行環境 (`host_commands` / `env` / `capabilities` / `allowed_domains`) を machine 単位でまとめる機能です。`workspaces` テーブルで DB 管理され (Phase 2.5)、`default` workspace は daemon 起動時に常に自動生成されます。project を登録すると自動的に `default` に割り当てられ、`boid project init/add --workspace <slug>` は get-or-create (未知の slug でも空の workspace を自動作成してから紐付け)。各 workspace は永続的な `$HOME` (workspace home) を持ち、ツールチェーンの設置は `additional_bindings` (撤去済み) ではなく [`init.sh`](../guide/workspace-home.md) で行う。

> **slug に `export` / `apply` は使わないこと。** この 2 つは HTTP API 側で workspace 名と同じ階層の静的ルートになっているため、その名前の workspace は `boid workspace show` / `edit` / `remove` から到達できなくなる (`exactly one of ?all=true or ?name=<slug> is required` という、指定した覚えのないクエリパラメータの話をするエラーになる)。作成自体はできてしまう。予約語として弾いていないのは意図的な判断 (2026-07-28)。
>
> 既に作ってしまった場合の回避策:
>
> - **定義を読む**: `boid workspace show <slug>` は使えない (フラグも無い)。`boid workspace export <slug>` を使う — こちらは `?name=` 経由なので通る
> - **編集・削除**: `--force` を付ければ `edit` / `remove` は通る。ただし `--force` は `edit` の If-Match による競合検出と `remove` の削除確認プロンプトを両方とも無効にするので、下のリネーム手順以外では使わないこと
> - **リネーム** (推奨): `boid workspace export <slug>` で書き出す → `metadata.name` を衝突しない名前に書き換える → `boid workspace apply -f` で新しい名前を作る → `boid workspace remove <旧 slug> --force` で消す。`boid workspace edit` ではリネームできない (slug は path パラメータで、body に `slug:` を書いても strict デコーダが弾く)
>
> `create` / `list` / `apply` / `export` と `<slug>` 配下のサブコマンド (`get-init-script` など) は影響を受けない。

| コマンド | 役割 |
|---|---|
| `boid workspace list` | ワークスペース一覧 |
| `boid workspace show <slug>` | 定義 (host_commands/env/capabilities) と割り当て済み project を表示 |
| `boid workspace create <slug> [--from-file <yaml>]` | 新規作成 (`--from-file` 省略時は空の workspace)。`--from-file` が取るのは **meta の平の mapping** (`env:` / `host_commands:` 等が top-level)。 `boid workspace export` が書く envelope 文書 (`apiVersion:` / `kind:` 付き) は受け付けない — そちらは `boid workspace apply -f` を使う |
| `boid workspace edit <slug> --from-file <yaml>` | 既存 workspace を丸ごと置き換え (自動 If-Match、`--force` で last-write-wins)。`--from-file` の形式は `create` と同じ (envelope は不可) |
| `boid workspace import <file>` | **廃止 (2026-07-28)、hidden コマンド**。meta 形式の export/import round trip が双方向とも壊れていたため envelope 形式に一本化した (詳細は [volume-only-daemon.md 論点g](../../plans/volume-only-daemon.md))。実行すると常にエラーで終了し、ファイルの形式に応じて `boid workspace apply -f <file>` (envelope 文書) または `boid workspace create/edit <slug> --from-file <file>` (bare meta 文書) を案内する |
| `boid workspace export <slug>\|--all [-o FILE]` | workspace (+ 割り当て済み project 群の name/url + `spec.init_script` = workspace の `init.sh` 全文) を `apiVersion: boid.dev/v1 / kind: Workspace` の yaml として書き出す (省略時 stdout)。`--all` で全 workspace を 1 file に `---` 区切りでまとめて書き出す — **`boid workspace export --all` が唯一の正式 backup 経路** (DB の生コピーは復元手段として不十分。詳細は [volume-only-daemon.md 論点g](../../plans/volume-only-daemon.md))。 `boid workspace apply` が読めないサイズの文書になる場合は、書き出さずに該当 workspace 名を挙げて失敗する — **成功した export は必ず apply できる**。 同じ理由で、応答の文書が `spec.init_script` を持たない場合 (= daemon が PR9 以前で init.sh を知らない) も、**init.sh の入っていない backup を書かずに失敗する** — daemon を upgrade してから再実行する |
| `boid workspace apply -f FILE [--dry-run]` | `boid workspace export` が出力した yaml を適用 (upsert: 未知の slug は新規作成、既存 slug はフィールド単位でマージ — 省略フィールドは現状維持、明示的な空値は clear)。`spec.projects[]` は名前が一致する既存 project への割当のみ行う (URL からの新規登録は PR-2 待ち)。`spec.init_script` があれば workspace の `init.sh` も復元する (明示的な空文字列は削除、キー自体が無ければ現状維持 — DB の transaction とは別コミットなので、metadata だけ適用されて init.sh が失敗した場合はその旨をエラーで報告する)。`--dry-run` で書き込みなしのプレビューのみ |
| `boid workspace assign <project-ref> <workspace-id>` | プロジェクトをワークスペースに紐付け (未知の slug は 404。ただしローカル `workspace.yaml` が存在すればそこから auto-create) |
| `boid workspace clear <project-ref>` | プロジェクトの紐付けを `default` にリセット |
| `boid workspace import-home <slug> [--from DIR] [--dry-run] [--yes\|--force]` | volume 化以前のホスト側 workspace home (既定 `~/.local/share/boid/homes/<slug>`) の中身を、その workspace の HOME volume へ移行する。CLI が DIR を **読んで** tar にし daemon にストリーム、daemon が volume を作り直して展開する (bind mount は使わない — rootless podman の uid mapping を跨げないため)。`0600` などの mode は維持。**移行元は読むだけで削除も変更もしない**。job が走っている workspace は 409 で拒否 (この時点では何も壊れていない)。既存 volume は破棄されるため既定で確認プロンプト。移行後は次の dispatch で `init.sh` が 1 回走る (仕様)。詳細は [workspace home ガイド](../guide/workspace-home.md#旧-workspace-home-ホスト側ディレクトリ-の移行) |
| `boid workspace get-init-script <slug> [-o FILE]` | workspace の `init.sh` を表示 (`-o` でファイルに保存)。 **init.sh は daemon が保持している** — 実体は daemon の `~/.config/boid/workspaces/<slug>/init.sh` だが container デプロイでは daemon の state volume 内なので、ホスト側の同名 path を編集しても反映されない。 init.sh 未設定の workspace は非ゼロ終了 |
| `boid workspace set-init-script <slug> -f FILE [--force]` | ローカルのファイル (`-` で stdin) を workspace の `init.sh` として登録。 書き込み前に現在のリビジョンを If-Match で確認する (`--force` で無効化)。 **空のファイルは「init.sh 無し」として削除扱い** になる (空のスクリプトと未設定は dispatch から見て同じ状態)。 上限 **128 KiB** — 「受理した script は `export` → `apply` で必ず戻せる」から逆算した値。 apply する文書の `spec.init_script` にも同じ上限が効くので、どちらの経路からもこれを超える script は保存できない |
| `boid workspace edit-init-script <slug> [--force]` | `init.sh` を `$EDITOR` (既定 vi) で編集し、保存時に適用。 未設定なら空バッファで始まる。 適用に失敗した場合は編集内容を temp file に残してその path を報告する |
| `boid workspace unset-init-script <slug> [--force]` | workspace の `init.sh` を削除し、script 実行なしの状態に戻す。 HOME volume とその中身には触れない |
| `boid workspace remove <slug> [--force\|--yes]` (alias: `delete`) | ワークスペースを削除 (割り当て済み project は `default` へ再割当。`default` 自体は削除不可)。home volume と **workspace の `init.sh`** も削除する (どちらも best-effort — 失敗しても remove 自体は成功し、warning で報告する)。home のサイズ表示付き確認プロンプトが出る (`--force`/`--yes` でスキップ)。詳細は [workspace home ガイド](../guide/workspace-home.md#workspace-の削除) |
| `boid workspace services add <slug> <service> [service...]` | workspace 単位で API gateway service を有効化 (docs/plans/api-gateway.md §3)。GET で現在の `Services` を取得 → 追記 (重複は無視) → `spec.services` のみを持つ最小 envelope を `POST /api/workspaces/apply` — `task_behaviors`/`host_commands` など他フィールドは一切触らない (`edit` の PUT 経路は `workspaceMetaStrict` に `task_behaviors` 等の field が無いため使えない — 使うと 400 になるか、対応していたとしても他フィールドを空に巻き戻してしまう) |
| `boid workspace services remove <slug> <service> [service...]` | 有効化済み API gateway service を無効化。存在しない名前を指定してもエラーにならない (no-op) |
| `boid workspace services list <slug>` | workspace 自身が追加した API gateway service 一覧を表示 (config.yaml `services_floor` 側は含まない — `allowed_domains` の `show` が floor を表示しないのと同じ設計) |

## Host Commands

daemon が集約する `~/.config/boid/host_commands.yaml` (workspace 群の `host_commands` を preflight 時に集約した設定) を確認・再読込します (Phase 2.5 PR4)。

| コマンド | 役割 |
|---|---|
| `boid host-commands list` | daemon が把握している host_commands の名前一覧 |
| `boid host-commands reload` | `host_commands.yaml` を手で編集した後に daemon に再読込させる |

## サンドボックス操作

| コマンド | 役割 |
|---|---|
| `boid exec -p <project-ref> [--name NAME] [--readonly] -- <argv...>` | サンドボックス内で任意の argv を実行。 project の `host_commands` / `env` を継承する (`additional_bindings` は Phase 4 PR4 で撤去済み、workspace home に依存)。 `--` 以降が sandbox 内の argv (旧 `commands:` 名前指定は Phase 3-d で廃止)。 `--name` でジョブの表示名、 `--readonly` でワークスペースを read-only に |
| `boid attach <job-id>` | 実行中のジョブの runtime に attach (interactive ジョブ向け) |
| `boid fetch <url>` | URL のコンテンツをホスト側で取得して出力する (直接 HTTP アクセスが制限されているサンドボックス内から使用可) |

## エージェント

実行中のエージェントジョブを操作します。

| コマンド | 役割 |
|---|---|
| `boid agent claude  -p <project> [--instruction "..."] [--readonly] [--model M] [--name NAME] [--no-attach]` | claude セッションをサンドボックス内で起動し PTY に attach する。セッションは常に新規プロセス (session-id での再開は撤去済み、`cmd/agent_session.go`)。 `--no-attach` で job-id だけ表示して終了 |
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
