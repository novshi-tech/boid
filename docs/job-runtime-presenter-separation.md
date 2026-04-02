# Job Runtime / Presenter Separation

## Goal

`boid` の job 実行基盤から `tmux` を外し、
process management と presentation を分離する。

ここでいう分離は、
「`tmux` をやめる」ことではなく、
「`tmux` を execution の必須条件から外す」ことを指す。

狙いは以下。

- job 実行の信頼性を `tmux` 依存から切り離す
- interactive LLM session を起動時から PTY 付きで動かせるようにする
- `tmux` / CLI attach / Web UI など複数 presenter を同じ job にぶら下げられるようにする
- 将来の rich UI 拡張を可能にする

## Current Problem

現在の dispatcher は、
概ね以下の構造になっている。

1. `tmux new-window` を実行する
2. その window の中で sandbox/job を起動する
3. hook の完了を待ち、job state を更新する

この構造だと以下の問題がある。

- `tmux` 起動失敗が job 起動失敗になる
- process runtime と terminal UX が密結合している
- 実行状態の観測手段が実質 `tmux` window に固定される
- Web UI など別 presenter を足しにくい
- real `tmux` 固有の失敗が mock 中心の test から漏れやすい

今回実際に起きた `tmux new-window` の target 指定不具合は、
この結合の弱さをそのまま露呈した。

## Current Boundary Snapshot

移行前の責務境界を明示しておく。
ここが曖昧なまま進めると、
`tmux` を剥がす途中で責務が再混線する。

### `Runner`

- job record を作る
- broker token を登録する
- sandbox launch spec を組み立てる
- 実行を起動し、job 完了待ちの受け口を持つ

### `sandbox`

- 実行境界を作る
- sandbox script を生成する
- hook/gate を child process として起動できる形にする

### `tmux`

- 現状は presenter ではなく execution bootstrapper になっている
- `new-window` 失敗がそのまま job 起動失敗になる
- 実行中 job の観測経路を tmux window に固定している

### `boid job done`

- hook/gate 完了を server に返す completion callback である
- runtime 自体の child exit wait ではなく、
  sandbox 内からの completion 契約として使われている
- 移行中も「job 完了時に server 側 state machine を進める」契約は維持する

## Key Insight

interactive な `codex` / `claude code` に必要なのは
`tmux` ではなく `PTY` である。

重要な整理:

- job は起動時から PTY を必要とすることがある
- しかし PTY の供給者が `tmux` である必要はない
- `tmux` は PTY を見せる presenter の 1 つになれる

つまり、後から足せるのは `tmux` であって、
後から生やせるのは `TTY` ではない。

したがって、
core runtime が最初から PTY を確保して job を起動し、
その PTY に対して `tmux` や Web UI が attach する構造が自然になる。

## Proposed Architecture

### 1. Core Runtime

core が持つ責務は以下に限定する。

- child process 起動
- sandbox 起動
- PTY 確保
- stdin/stdout/stderr の中継
- exit code / completion / cancel / timeout 管理
- transcript / output の保存

この層は presenter を知らない。

## 2. Presenter

presenter は、
既に動いている job runtime を人間に見せる責務だけを持つ。

候補:

- `tmux`
- `boid attach <job-id>`
- Web UI
- transcript / replay viewer

`tmux` はこの層の 1 実装にする。

つまり、
`tmux` が失敗しても job 自体は継続できる構造にする。

## 3. Hook / Gate

hook / gate は「何を実行するか」を持つ。

例:

- `codex exec`
- `codex resume`
- `claude code`
- host command gate

job runtime はこれらを process として起動するだけで、
presentation には関与しない。

## Concrete Model

### Current

```text
dispatcher
  -> tmux new-window
       -> sandbox
            -> hook script
                 -> codex exec
```

### Proposed

```text
dispatcher
  -> job runtime
       -> PTY を確保して sandbox/job を起動
       -> job id に紐づく PTY / transcript を保持

presenter
  -> boid attach <job-id>
  -> tmux window から attach
  -> Web UI terminal stream
```

この場合、
`tmux` は execution path ではなく attach path に降りる。

## CLI Direction

最小の user-facing entrypoint は以下。

```text
boid attach <job-id>
```

期待する役割:

- job の PTY に接続する
- 入出力をそのまま中継する
- attach / detach を許容する
- job 自体は attach の有無に依存しない

この上で、
`tmux` は薄い wrapper にできる。

```text
tmux new-window -n job-xxxx "boid attach <job-id>"
```

こうすれば `tmux` 失敗は viewer failure であって、
job execution failure ではなくなる。

## Benefits

### Reliability

- `tmux` 失敗で dispatch が壊れない
- job lifecycle と UI lifecycle を独立して扱える
- headless 環境でも interactive job runtime を持てる

### Extensibility

- Web UI に live terminal を直接描画できる
- transcript 保存と replay がやりやすい
- presenter を複数持てる
- 将来 `tmux` 以外の UI を足しやすい

### Testability

- runtime は PTY / process 管理として単体テストできる
- presenter は別途 e2e/smoke で検証できる
- real `tmux` 依存の失敗を core execution と分離できる

## Tradeoffs

### New complexity in runtime

core が PTY を持つので、
以下の管理が新たに必要になる。

- PTY lifecycle
- terminal resize
- transcript buffering
- attach 時の fan-out

ただしこれは `tmux` に execution を委譲して曖昧にするより、
責務として明確である。

### Attach semantics must be designed

以下は仕様が必要。

- 同時 attach を許すか
- write 権限を 1 client に制限するか
- transcript をどこまで保持するか
- job 完了後 attach を replay にするか

## Migration Plan

### Phase 1: Stabilize current system

現行 tmux 依存を維持したまま、
job / dispatch / migration 周りを安定化する。

これは今回の修正で一部前進した。

### Phase 2: Introduce runtime-backed attach

- job runtime が PTY を持てるようにする
- `boid attach <job-id>` を追加する
- transcript / stream API を用意する

この時点では `tmux` はまだ optional でよい。

### Phase 3: Move tmux to presenter

- current `tmux new-window "bash outer.sh"` をやめる
- 必要なら `tmux new-window "boid attach <job-id>"` を使う
- dispatcher は presenter の失敗で job を失敗させない

### Phase 4: Add Web presenter

- Web UI から live terminal を表示する
- attach / observe / replay を提供する

## Implementation Breakdown

別セッションで順に進められるよう、
実装タスクを phase ごとに分解する。

### Phase 0: Frame the boundary

目的:

- 移行中の責務ぶれを防ぐ
- 現状の問題を issue として固定する

タスク:

1. `Runner` / `sandbox` / `tmux` / `job done` の責務境界を短く文書化する
2. 現行 tmux 経路の real-world failure mode を issue 化する
3. existing E2E が real `tmux` execution path を十分に守っていないことを明記する

### Phase 1: Introduce a runtime abstraction

目的:

- dispatcher から `tmux` 直参照を外す
- execution path を presenter path から切り離す準備をする

タスク:

1. `JobRuntime` interface を導入する
2. 責務を `Start`, `Attach`, `Resize`, `Wait`, `Stop` 程度に限定する
3. job に `interactive`, `tty`, `runtime_id` 相当の metadata を追加する
4. 現行 tmux 起動は adapter 実装として一旦包む

### Phase 2: Build a PTY-backed runtime

目的:

- core が最初から PTY を持って job を起動できるようにする

タスク:

1. PTY 付き local process runtime を実装する
2. stdout/stderr transcript を保存する
3. exit code / completion / cancel を runtime 側で管理する
4. attach 用の stream API を用意する

備考:

- この段階では sandbox なしの単純な child process でもよい
- まず PTY lifecycle を core が持てることが重要

### Phase 3: Add attach CLI

目的:

- `tmux` なしでも interactive job を観測・操作できるようにする

タスク:

1. `boid attach <job-id>` を追加する
2. attach 時に PTY の入出力をそのまま中継する
3. detach semantics を決める
4. terminal resize の最小対応を入れる
5. `boid job show` に attachability を表示する

### Phase 4: Move sandbox hook execution off tmux

目的:

- sandbox hook の execution path から `tmux` を外す

タスク:

1. `Runner.launchSandbox()` が直接 `tmux new-window` しない構造に変える
2. sandbox hook job を PTY-backed runtime 上で起動する
3. 既存の `boid job done` completion 契約を維持したまま移行する
4. cancel / cleanup / failure propagation を見直す

### Phase 5: Reintroduce tmux as a presenter

目的:

- `tmux` を optional presenter に落とす

タスク:

1. `tmux new-window "boid attach <job-id>"` 型の wrapper を提供する
2. `tmux` failure を viewer failure として扱う
3. job execution failure と混同しない logging / status を整える

### Phase 6: Add Web presenter

目的:

- Web UI から live terminal / replay を提供する

タスク:

1. read-only terminal stream API を追加する
2. Web UI で live terminal を表示する
3. transcript replay を実装する
4. 必要なら後続で Web input を追加する

## Suggested Issue Order

実作業の着手順としては以下が安全。

1. `JobRuntime` interface を追加し、dispatcher から `tmux` 直参照を外す
2. PTY 付き local process runtime を実装する
3. `boid attach <job-id>` の最小 CLI を追加する
4. transcript 保存と `job show` の表示を追加する
5. sandbox hook job を new runtime に移行する
6. `tmux presenter` を `attach` wrapper に置き換える
7. Web terminal read-only presenter を追加する
8. real `tmux` / attach / interactive job の E2E を追加する

## Recommended First Slice

最初の 1 セッションで欲しい最小成果は以下。

1. `JobRuntime` interface 導入
2. `boid attach <job-id>` の最小 CLI
3. PTY 付き local runtime
4. hook sandbox の移行に必要な接続点の洗い出し

この順なら、
途中で止まっても構造改善が前に進む。

## Non-Goals

この note の段階ではまだ決めない。

- Web terminal protocol の具体実装
- transcript persistence の最終 format
- multi-user terminal arbitration policy
- `codex` / `claude code` ごとの差分吸収方法

## Summary

`tmux` は必要になりうるが、
job execution の必須基盤に置くべきではない。

interactive LLM session に必要なのは起動時からの PTY であり、
`tmux` はその PTY を見せる presenter の 1 つとして扱うのが自然である。

`boid` core は runtime に徹し、
`tmux` / CLI attach / Web UI は presenter として分離する。

この構造にすると、
信頼性、観測性、将来の UI 拡張性のすべてが改善する。
