# テスト設計ガイドライン

## 何をテストすべきか

### 正常系フロー（必須）

- タスクが期待ステータス（`done` 等）に到達すること
- artifact の内容が正しいこと
- プロジェクト登録・タスク作成が成功すること

### 状態遷移（新機能追加時）

- `pending` → `executing` → `done` の遷移が正しく行われること
- entry/exit gate の発火タイミングと exit code が遷移をどう変えるか
- 並列 hook と順次 hook の実行順序

### エラーケース（任意、複雑化する場合は省略可）

- exit gate が fail して done への遷移をブロックする (executing に留まる)
- abort アクションによるタスク中断 (`aborted` 終端状態)

## アサーションの書き方

`e2e_assert_contains <haystack> <needle>` を使う。

```bash
# タスクの JSON レスポンスを変数に保存
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"

# ステータスのアサーション
e2e_assert_contains "$task_json" '"status":"done"'

# artifact の内容のアサーション
e2e_assert_contains "$task_json" '"artifact"'
e2e_assert_contains "$task_json" '"result":"done"'

# プロジェクト一覧のアサーション
project_list="$("$E2E_BIN_DIR/boid" project list)"
e2e_assert_contains "$project_list" "my-scenario"
```

**注意**: `e2e_assert_contains` は部分文字列マッチ。JSON キーが正しく含まれるかを確認する程度に使う。

## 非同期処理の待機パターン

### タスクステータスの待機

```bash
# 最大 20 秒、100ms 間隔でポーリング
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" executing)"
```

ステータス値: `pending`, `executing`, `done`, `aborted`

### ジョブ数の待機

```bash
# ジョブが 2 件以上になるまで待機（hook 2 つが起動した状態）
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
```

### ジョブのロール別件数検証

```bash
# hook が 2 件、gate が 0 件であることを確認
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" gate 0
```

### ファイルの出現待機

```bash
# agent-a が実行後に書き出すファイルを待機（e2e/lib/common.sh の関数）
e2e_wait_for_file "$PROJECT_DIR/agent-a-instructions.json"
```

## fake コマンドの作り方（hostbin パターン）

host_commands を使うシナリオでは、実際の外部コマンド（`gh`, `git`, `systemctl` 等）の代わりに
fake スクリプトを使う。

### 配置場所

`e2e/fixtures/hostbin/` に置く。`run.sh` が自動的に `$E2E_BIN_DIR` にコピーし、
`$PATH` の先頭に追加されるため fake が優先実行される。

### fake スクリプトのテンプレート

```bash
#!/usr/bin/env bash
set -euo pipefail

log_file="${E2E_STATE_DIR:?}/fake-gh.log"
{
  printf 'cmd=gh\n'
  printf 'cwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf -- '---\n'
} >>"$log_file"

# 必要に応じて stdout に出力（コマンドの戻り値をシミュレート）
printf 'https://example.invalid/pr/123\n'

exit 0
```

### fake ログの検証

```bash
# fake-gh.log に期待するコマンドが記録されているか確認
[[ -f "$E2E_STATE_DIR/fake-gh.log" ]] || e2e_fail "missing fake gh log"
grep -F 'args=pr create --title My PR' "$E2E_STATE_DIR/fake-gh.log" \
  >/dev/null || e2e_fail "gh pr create was not invoked"
```

### kit.yaml での host_commands 宣言

```yaml
host_commands:
  gh:
    path: ${E2E_BIN_DIR}/gh   # fake コマンドのパス
    allow:
      - pr                     # 許可するサブコマンド
  systemctl:
    path: ${E2E_BIN_DIR}/systemctl
    allow:
      - restart
```

## builtin command (boid / git) のテスト

新しい builtin op を追加した場合は、 hook / gate スクリプトから sandbox 経由で呼ぶ E2E を入れること。 `boid task list` のようにテキスト出力する builtin は scenario.sh から host CLI として呼んでも別物なので、 sandbox 内で発火させる経路を組む。

参照: `e2e/scenarios/builtin-task-create/` — gate スクリプト内で `boid task create` を呼ぶパターンを採用している。

## サンドボックス前提条件の扱い

`requires-sandbox` マーカーファイルを置くだけでよい（内容は空で可）。

```bash
touch e2e/scenarios/my-scenario/requires-sandbox
```

これにより `run.sh` が以下を確認する:
- `pasta` コマンドが存在するか
- `unshare` コマンドが存在するか  
- `nft` コマンドが存在するか
- `unshare --user --mount --map-root-user` が成功するか

**サンドボックスが必要なシナリオ**: hostcmd（ホストコマンドブローカー）を使うもの。
`requires-sandbox` なしのシナリオは CI でも開発マシンでも実行できる。

## 既存シナリオの参照

| シナリオ | 特徴 | 参照ポイント |
|---------|------|------------|
| `project-smoke` | 最小構成、サンドボックス不要 | シンプルなプロジェクト登録 + アサーション |
| `readonly-hook-gate` | 並列 hook + exit gate 構成 | hook/gate 同期パターン |
| `phase-dependency` | 複数子タスクと `artifact.children.all_done` による phase 連結 | 親子 / 依存関係 / abort 動作 |
| `builtin-task-create` | hook / gate からの `boid task create` builtin | サブタスク生成の現行パターン (旧 tasks trait の代替) |
| `task-import-smoke` | JSONL からの一括 import (`boid task import`) | bulk task 投入 |
| `host-command-smoke` | hostcmd (gh, systemctl)、サンドボックス必須 | fake コマンドの使い方 |
