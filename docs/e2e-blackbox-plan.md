# E2E Black-Box Test Plan

## Status Snapshot

2026-04-02 時点の実装状況を残す。

完了済み:

- Phase 1: `boid start` に `--db-path` `--socket-path` `--http-addr` `--tmux-session` `--kits-dir` `--key-file-path` を追加済み
- Phase 2: `e2e/run.sh`、`e2e/lib/common.sh`、`e2e/cmd/boid-e2e` を追加済み
- helper は `wait-unix-socket` `wait-health` `get-task` `wait-task-status` `list-jobs` を実装済み
- `project-smoke` で isolated temp root 上の server 起動と project 登録を確認済み
- `host-command-smoke` で fixture gate から host command を呼び、task payload へ `artifact` / `verification` が反映されることを確認済み

未完了:

- `TODO-hook-gate.md` を埋める本命 scenario 群は未着手
- CI 組み込みは未着手
- product 側の `job list/show/watch` 追加は未着手

現時点の black-box E2E は「基盤と最小契約の確認」までは通っているが、
hook/gate 並列実行や rework loop を守る段階にはまだ入っていない。

## Decision

boid の E2E は `go test` の延長としては扱わない。

代わりに、

- 実際の `boid` バイナリを起動する
- 実際の `boid` CLI を叩く
- 実際の tmux / sandbox / broker / DB を使う
- ただし DB / kits / socket / tmux server は専用環境へ隔離する

という black-box 方式で実行する。

## Why This Approach

現在のレイヤ統合テストは、
script 生成や server の smoke test まではカバーしているが、
実際の `boid start` -> dispatch -> sandbox -> tmux -> job completion
の一連経路をまとめて検証する層はない。

また、ホスト上の既存環境と以下が衝突しうる。

- `~/.local/share/boid/boid.db`
- `~/.local/share/boid/kits`
- runtime socket
- tmux session
- ホスト上の本物の `git` / `gh` / `systemctl`

ただし、これは専用 sandbox をもう一段外側に作らなくても、
環境変数と `boid start` の設定注入で大部分を隔離できる。

## Core Idea

E2E harness は一時ディレクトリを作り、
その中に boid の実行状態を閉じ込める。

最低限隔離するもの:

- `HOME`
- `XDG_DATA_HOME`
- `XDG_RUNTIME_DIR`
- `BOID_SOCKET`
- `TMUX_TMPDIR`
- `PATH`

これにより、

- DB は専用パス
- kits は専用パス
- UNIX socket は専用パス
- tmux server socket も専用パス
- fake host command を優先実行

が実現できる。

## Product Changes Required

E2E を安定して回すために、
まず product 側に最小限の起動パラメータを追加する。

## 1. `boid start` をパラメータ化する

### Add flags

- `--db-path`
- `--socket-path`
- `--http-addr`
- `--tmux-session`
- `--kits-dir`
- `--key-file-path`

必要に応じて追加候補:

- `--allowed-domain` を repeatable flag 化

### Defaults

現行挙動は維持する。
flag 未指定時は現行の default を使う。

### Why

現状 `boid start` は以下を固定している。

- DB path は `XDG_DATA_HOME` から導出
- kits dir も `XDG_DATA_HOME` から導出
- socket は `BOID_SOCKET` / `XDG_RUNTIME_DIR` ベース
- `HTTPAddr` は `:8080`
- `TmuxSession` は `boid`

DB/kits/socket は env 差し替えで十分だが、
`HTTPAddr` と `TmuxSession` は flag で明示指定できた方が
E2E harness を単純にできる。

## 2. `boid stop` は E2E では使わない

現状の `boid stop` は `/api/shutdown` を呼ぶが、
対応 route が存在しない。

したがって E2E harness では stop command に依存せず、
起動した `boid start` プロセスへ `SIGTERM` を送る。

`boid stop` の修正は別トピックとして扱う。

## Test Harness Structure

## Entry Point

E2E の entrypoint は shell script にする。

例:

- `e2e/run.sh`
- `e2e/run.sh readonly-hook-gate`
- `e2e/run.sh --keep-temp feedback-loop-full`

理由:

- black-box 的で分かりやすい
- CI とローカル実行の差が小さい
- 失敗時に temp dir を残しやすい

## Helper

シナリオ本体は shell でよいが、
待機や JSON 検証は shell だけだと不安定になる。

そのため補助用に小さな helper を用意する。

例:

- `e2e/cmd/boid-e2e`

この helper は public API だけを使う。
DB 直接参照は禁止する。

想定サブコマンド:

- `wait-health`
- `wait-task-status`
- `wait-job-count`
- `get-task`
- `list-jobs`
- `assert-task-status`
- `assert-job-role-count`
- `assert-payload-has`

役割は「シナリオを読みやすくすること」であり、
product 機能の代替ではない。

## Temp Root Layout

各シナリオは専用 temp root を持つ。

例:

```text
$TMP/e2e-<scenario>-<rand>/
  bin/
    boid
    boid-e2e
    git
    gh
    systemctl
  home/
  data/
    boid/
      boid.db
      kits/
  run/
    boid.sock
  tmux/
  logs/
    server.stdout.log
    server.stderr.log
    scenario.log
  state/
    fake-git.log
    fake-gh.log
    fake-systemctl.log
  workspace/
    <projects copied from fixtures>
```

環境変数の設定:

- `HOME=$ROOT/home`
- `XDG_DATA_HOME=$ROOT/data`
- `XDG_RUNTIME_DIR=$ROOT/run`
- `BOID_SOCKET=$ROOT/run/boid.sock`
- `TMUX_TMPDIR=$ROOT/tmux`
- `PATH=$ROOT/bin:$PATH`
- `E2E_ROOT=$ROOT`
- `E2E_STATE_DIR=$ROOT/state`
- `E2E_BIN_DIR=$ROOT/bin`

## Fixture Strategy

## 1. kits は本物ではなく fixture を使う

`github.com/novshi-tech/boid-kits` 本体を clone しない。

E2E の目的は kit repository の配布確認ではなく、
core と kit の契約確認だからである。

fixture kits は以下のように置く。

```text
e2e/fixtures/kits/github.com/novshi-tech/boid-kits/
  readonly/
    kit.yaml
    hooks/
    gates/
  feedback/
    kit.yaml
    hooks/
    gates/
  worktree/
    kit.yaml
    hooks/
    gates/
```

harness はこれを
`$XDG_DATA_HOME/boid/kits/...` にコピーする。

これにより、
project 側は本番と同じ ref を使える。

例:

- `github.com/novshi-tech/boid-kits/readonly`
- `github.com/novshi-tech/boid-kits/feedback`

## 2. host command は fake にする

`git` / `gh` / `systemctl` は本物を叩かない。

fixture command を `$ROOT/bin/` に置き、
kit 側の `host_commands.<name>.path` は
`${E2E_BIN_DIR}/git` のように指定する。

`host_commands.path` は env 展開されるため、
fixture への絶対パスを project/kit YAML に埋め込まずに済む。

fake command の責務:

- 引数を記録する
- 必要なら stdin を記録する
- 成功/失敗を切り替えられる
- 結果を `verification` / `artifact` 側で使える最小情報だけ返す

例:

- fake `git push`: 引数を `fake-git.log` に追記して exit 0
- fake `gh pr create`: 固定 PR URL を stdout に出す
- fake `gh pr view --json ...`: 固定 review state を返す
- fake `systemctl restart boid`: `fake-systemctl.log` に記録して exit 0

補足:

- 現在の `host-command-smoke` は最小契約確認のため `git` を使わない
- smoke で実際に使う fake host command は `gh` と `systemctl` のみ
- fake `git` は将来の `push` 含み scenario 用 fixture として残してよい

## 3. project fixture は scenario ごとに持つ

各 scenario は workspace fixture を持つ。

例:

```text
e2e/scenarios/
  readonly-hook-gate/
    workspace/
      app/
        .boid/project.yaml
        .boid/project.local.yaml
  writable-worktree/
    workspace/
      boid/
        .boid/project.yaml
        .boid/project.local.yaml
```

`project.local.yaml` は E2E 向け上書きに使ってよい。

用途:

- fixture kit の追加
- fake host command path の上書き
- E2E 用 env の注入
- 追加 bind mount の注入

## Scenario Design Rules

各 scenario script は次の原則に従う。

### Allowed

- `boid project add`
- `boid workspace assign`
- `boid project reload`
- `boid task create`
- `boid action send`
- `boid exec`
- `boid-e2e` helper

### Forbidden

- DB を直接読む
- SQLite を直接書く
- tmux state を直接成功判定に使う
- kit fixture を scenario 実行中にその場生成する

### Success Criteria

成功判定は public surface を通す。

例:

- task status
- job count / role / status
- task payload
- fake command のログ

## Harness Responsibilities

`e2e/run.sh` は以下を担う。

1. `boid` バイナリを build する
2. `boid-e2e` helper を build する
3. temp root を作る
4. fixture kits / projects / fake commands をコピーする
5. env を組み立てる
6. `boid start` を background 起動する
7. `wait-health` で readiness を待つ
8. scenario script を実行する
9. 終了時に `SIGTERM` で server を止める
10. 失敗時は logs / temp root を残す

## Recommended File Layout

```text
e2e/
  run.sh
  lib/
    common.sh
  cmd/
    boid-e2e/
      main.go
  fixtures/
    kits/
    hostbin/
  scenarios/
    readonly-hook-gate/
      scenario.sh
      workspace/
    writable-chain/
      scenario.sh
      workspace/
    rework-cycle/
      scenario.sh
      workspace/
    feedback-loop-full/
      scenario.sh
      workspace/
```

## `boid start` Invocation In E2E

E2E harness からの起動例:

```bash
boid start \
  --db-path "$XDG_DATA_HOME/boid/boid.db" \
  --socket-path "$BOID_SOCKET" \
  --http-addr "127.0.0.1:0" \
  --tmux-session "boid-e2e-${SCENARIO_ID}" \
  --kits-dir "$XDG_DATA_HOME/boid/kits"
```

この形にしておけば、

- 既存 DB と衝突しない
- 既存 kits と衝突しない
- socket 衝突しない
- tmux session 衝突しない
- HTTP port 衝突しない

## Debugging And Failure Artifacts

E2E は失敗時の調査性が重要なので、
以下を常に保存する。

- server stdout
- server stderr
- scenario stdout/stderr
- fake host command logs
- temp root path

追加であるとよいもの:

- task list dump
- task detail dump
- job list dump
- `tmux list-windows`
- 必要なら `tmux capture-pane`

`--keep-temp` 指定時は temp root を削除しない。
失敗時はデフォルトで temp root を残してもよい。

## Initial Implementation Order

## Phase 1: Make `boid start` Configurable

最小変更:

- `cmd/start.go` に起動 flag を追加
- 既定値は現行維持
- 単体テストを追加

完了条件:

- 任意 DB / socket / kits / tmux session / HTTP addr で起動できる

## Phase 2: Add E2E Harness Skeleton

追加物:

- `e2e/run.sh`
- `e2e/lib/common.sh`
- `e2e/cmd/boid-e2e`

完了条件:

- server を起動して health まで待てる
- fixture project を登録できる
- 終了時に clean up できる

進捗:

- 完了
- `project-smoke` で server 起動、health 待機、project 登録まで確認済み

## Phase 3: Add Common Fixture Kits And Fake Commands

追加物:

- `e2e/fixtures/kits/...`
- `e2e/fixtures/hostbin/...`

完了条件:

- fake `git` / `gh` / `systemctl` を core から呼べる
- PR 作成や restart の契約を実際に検証できる

進捗:

- 部分完了
- fixture kits と fake host command の配置、sandbox prerequisite check、helper の task/job 監視 API は追加済み
- `host-command-smoke` は `gh pr create` と `systemctl --user restart boid` だけを使う最小契約確認として通過済み
- `git push` を含む scenario はまだ作っていない

## Phase 4: Add First Four Scenarios

優先順:

1. readonly hook parallel + gate parallel + auto advance
2. writable hook sequential + gate + chain
3. manual abort + verification failed -> rework
4. feedback-loop full cycle

完了条件:

- `TODO-hook-gate.md` の未カバー項目を E2E で表現できる

次に着手する内容:

1. `readonly-hook-gate`
2. `writable hook + gate + chain`
3. `verification failed -> rework`
4. `feedback-loop full cycle`

この順で進める。
最初の 2 本は `TODO-hook-gate.md` の空白を埋めることを優先し、
`git push` のような外部連携よりも hook/gate の状態遷移確認を先に行う。

## Phase 5: CI Integration

例:

- `./e2e/run.sh`
- `./e2e/run.sh readonly-hook-gate`

CI では以下を明示する。

- `tmux` 必須
- `passt` 必須
- user namespace 必須

## First Scenario In Detail

最初に作るなら `readonly-hook-gate` がよい。

理由:

- worktree が不要
- fake host command も最小で済む
- hook 並列 / gate 並列 / auto advance を一気に見られる

シナリオ手順:

1. fixture project を workspace に配置
2. `boid project add`
3. `boid task create --behavior <readonly-one-shot>`
4. `boid action send --type start`
5. `boid-e2e wait-job-count`
6. `boid-e2e assert-job-role-count`
7. `boid-e2e wait-task-status done`
8. payload に expected artifact / verification があることを確認

## Non-Goals For The First Iteration

初回の E2E 基盤では以下はやらない。

- 本物の `boid-kits` clone/install
- 本物の GitHub API 連携
- 本物の `git push`
- systemd 実機連携
- E2E 自体をさらに外側 sandbox で囲う

まずは black-box だが deterministic な fixture ベースで、
core と kit の契約を固めることを優先する。

## Open Follow-Ups

後続で検討してよいもの:

- `boid stop` を正しく実装する
- `job list/show/watch` を product CLI に追加する
- E2E helper の一部を product CLI に還元する
- scenario 定義を shell から YAML/Go へ移すかの見直し

## Next Session Start Point

次セッションは以下から始めるとよい。

1. `TODO-hook-gate.md` の未カバー項目を scenario 名に落とす
2. `readonly-hook-gate` fixture project を追加する
3. `boid-e2e` に `wait-job-count` と `assert-job-role-count` を足す
4. `readonly-hook-gate` を先に通してから writable/rework へ進む
