# E2E インフラ概要

## run.sh の実行フロー

```
./e2e/run.sh [--keep-temp] [scenario]
```

1. `go build` で `boid` バイナリと `boid-e2e` ヘルパーをビルド
2. 対象シナリオを `e2e/scenarios/` から選択（引数なしで全実行）
3. シナリオごとに独立した tmpdir を作成し、以下を行う:
   - 環境変数をセット（HOME, XDG_DATA_HOME, BOID_SOCKET 等）
   - `e2e/fixtures/hostbin/` の fake コマンドを `$E2E_BIN_DIR` にコピー
   - `e2e/fixtures/kits/` の fixture kit を `$XDG_DATA_HOME/boid/kits/` にコピー
   - シナリオの `workspace/` を `$E2E_WORKSPACE_DIR` にコピー
   - boid サーバをバックグラウンド起動
   - `boid-e2e wait-health` でサーバ起動を待機
   - `scenario.sh` を実行
4. EXIT トラップでサーバを停止し tmpdir を削除（失敗時または `--keep-temp` 時は保持）

## e2e/lib/common.sh ヘルパー関数

| 関数 | 用途 |
|------|------|
| `e2e_log <msg>` | `[e2e] <msg>` を stdout に出力 |
| `e2e_fail <msg>` | `[e2e] ERROR: <msg>` を stderr に出力して exit 1 |
| `e2e_require_cmd <cmd>` | コマンドが存在しなければ e2e_fail |
| `e2e_require_sandbox_prereqs` | pasta/unshare/nft の存在確認（サンドボックス必須シナリオ用） |
| `e2e_assert_contains <haystack> <needle>` | 文字列が含まれなければ e2e_fail |
| `e2e_run <cmd> ...` | コマンドをログ出力してから実行 |
| `e2e_wait_for_file <path> [timeout] [interval]` | ファイルが現れるまで待機（デフォルト: 10秒, 0.05秒間隔） |

## boid-e2e ヘルパーコマンド

`e2e/cmd/boid-e2e/` でビルドされるテスト専用バイナリ。

| コマンド | 引数 | 説明 |
|---------|------|------|
| `wait-health [--timeout T] [--interval I] <socket>` | ソケットパス | /api/health が ok になるまで待機 |
| `get-task [--socket-path S] <task-id>` | タスクID | タスクの JSON を取得して stdout に出力 |
| `wait-task-status [--timeout T] [--interval I] [--socket-path S] <task-id> <status>` | タスクID, ステータス | タスクが指定ステータスになるまで待機し JSON を出力 |
| `list-jobs [--socket-path S] <task-id>` | タスクID | タスクのジョブ一覧 JSON を取得 |
| `wait-job-count [--timeout T] [--interval I] [--socket-path S] <task-id> <count>` | タスクID, 件数 | ジョブ数が count 以上になるまで待機 |
| `assert-job-role-count [--socket-path S] <task-id> <role> <count>` | タスクID, ロール, 件数 | 指定ロールのジョブ数を検証（不一致で exit 1） |

**role の値**: `hook`（hooks からのジョブ）、`gate`（gates からのジョブ）

## 環境変数

`run.sh` がシナリオの subshell にセットする変数:

| 変数 | 内容 |
|------|------|
| `E2E_ROOT` | シナリオ実行の tmpdir ルート |
| `E2E_STATE_DIR` | `$E2E_ROOT/state` — fake コマンドのログ出力先など |
| `E2E_BIN_DIR` | `$E2E_ROOT/bin` — boid, boid-e2e, fake コマンドの置き場所 |
| `E2E_LOG_DIR` | `$E2E_ROOT/logs` — サーバ・シナリオのログ |
| `E2E_WORKSPACE_DIR` | `$E2E_ROOT/workspace` — プロジェクトワークスペースのコピー先 |
| `BOID_SOCKET` | `$E2E_ROOT/run/boid.sock` — boid サーバの UNIX ソケットパス |
| `HOME` | `$E2E_ROOT/home` — 隔離された HOME |
| `PATH` | `$E2E_BIN_DIR:$PATH` — fake コマンドが優先される |

## サンドボックス要件

以下のシナリオは `requires-sandbox` マーカーが必要:
- サンドボックス内で実行するもの（ホストコマンドブローカー使用）
- `pasta`, `unshare`, `nft` が必要なもの

`requires-sandbox` ファイルが存在すると `run.sh` が `e2e_require_sandbox_prereqs` を呼び出す。
CI 環境（GitHub Actions）以外では実行できないシナリオはこれを使う。
